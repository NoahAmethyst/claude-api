package internal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bincooo/claude-api/types"
	"github.com/bincooo/claude-api/util"
	"github.com/bincooo/requests"
	"github.com/bincooo/requests/models"
	"github.com/bincooo/requests/url"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	WebClaude2BU = "https://claude.ai/api"
	UA           = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.0.0 Safari/537.36 Edg/114.0.1823.79"
	Mod          = "claude-2.0"
	Mod_V1       = "claude-2.1"
	//Mod_Magenta  = "claude-2.0-magenta"

	gRetry = 2
)

var (
	JA3       = "771,4865-4866-4867-49195-49199-49196-49200-52393-52392-49171-49172-156-157-47-53,0-23-65281-10-11-35-16-5-13-18-51-45-43-27-17513-21,29-23-24,0"
	cacheMods = make(map[string]_mod)
)

type _mod struct {
	time.Time
	model string
}

func _delMods() {
	for k, v := range cacheMods {
		// 10分钟内没有使用，则清理
		if v.Add(10 * time.Minute).Before(time.Now()) {
			delete(cacheMods, k)
		}
	}
}

func init() {
	JA3 = util.LoadEnvVar("JA3", JA3)
}

type webClaude2Response struct {
	Completion   string `json:"completion"`
	StopReason   string `json:"stop_reason"`
	Model        string `json:"model"`
	Truncated    bool   `json:"truncated"`
	Stop         string `json:"stop"`
	LogId        string `json:"log_id"`
	Exception    any    `json:"exception"`
	MessageLimit struct {
		Type string `json:"type"`
	} `json:"messageLimit"`
}

type WebClaude2 struct {
	mu sync.Mutex
	types.Options

	mod            string
	organizationId string
	conversationId string
}

func NewWebClaude2(opt types.Options) types.Chat {
	return &WebClaude2{mod: Mod, Options: opt}
}

func (wc *WebClaude2) NewChannel(string) error {
	return nil
}

func (wc *WebClaude2) Reply(ctx context.Context, prompt string, attrs []types.Attachment) (chan types.PartialResponse, error) {
	wc.mu.Lock()
	if wc.Retry < gRetry {
		wc.Retry = gRetry
	}

	if wc.mod == "" {
		wc.mod = Mod
	}

	if wc.organizationId == "" {
		if err := wc.getOrganization(); err != nil {
			wc.mu.Unlock()
			return nil, err
		}
	}

	if wc.conversationId == "" {
		if err := wc.createConversation(); err != nil {
			wc.mu.Unlock()
			return nil, err
		}
	}

	// 避免每次都检查新模型
	if mod, ok := cacheMods[wc.organizationId]; ok {
		wc.mod = mod.model
	}
	cacheMods[wc.organizationId] = _mod{time.Now(), wc.mod}

	var response *models.Response
	for index := 1; index <= wc.Retry; index++ {
		r, err := wc.PostMessage(5*time.Minute, prompt, attrs)
		if err != nil {
			if index >= wc.Retry {
				delete(cacheMods, wc.organizationId)
				wc.mu.Unlock()
				return nil, err
			}

			var wap *types.ErrorWrapper
			ok := errors.As(err, &wap)

			// 尝试新模型
			if ok && wap.ErrorType.Message == "Invalid model" {
				if wc.mod == Mod {
					logrus.Info("尝试新模型: ", Mod_V1)
					wc.mod = Mod_V1
				}
				cacheMods[wc.organizationId] = _mod{time.Now(), wc.mod}
			} else {
				logrus.Error("[retry] ", err)
			}
		} else {
			response = r
			break
		}
	}

	if response.StatusCode != 200 {
		return nil, errors.New(response.Text)
	}

	message := make(chan types.PartialResponse)
	go wc.resolve(ctx, response, message)
	return message, nil
}

func (wc *WebClaude2) resolve(ctx context.Context, r *models.Response, message chan types.PartialResponse) {
	defer wc.mu.Unlock()
	defer close(message)
	reader := bufio.NewReader(r.Body)
	block := []byte("data: ")
	original := make([]byte, 0)

	// return true 结束轮询
	handle := func() bool {
		line, hasMore, err := reader.ReadLine()
		original = append(original, line...)
		if hasMore {
			return false
		}
		//fmt.Println(string(original))
		if err == io.EOF {
			return true
		}

		if err != nil {
			message <- types.PartialResponse{
				Error: err,
			}
			return true
		}

		dst := make([]byte, len(original))
		copy(dst, original)
		original = make([]byte, 0)

		if !bytes.HasPrefix(dst, block) {
			return false
		}
		if !bytes.HasSuffix(dst, []byte("}")) {
			return false
		}

		dst = bytes.TrimPrefix(dst, block)
		var response webClaude2Response
		if err = CatchUnmarshal(dst, &response); err != nil {
			return false
		}

		message <- types.PartialResponse{
			Text:    response.Completion,
			RawData: dst,
		}

		if response.StopReason == "stop_sequence" {
			return true
		}

		return false
	}

	for {
		select {
		case <-ctx.Done():
			message <- types.PartialResponse{
				Error: errors.New("resolve timeout"),
			}
			return
		default:
			if handle() {
				return
			}
		}
	}
}

func (wc *WebClaude2) getOrganization() error {
	//headers := make(Kv)
	//headers["user-agent"] = UA
	response, err := wc.newRequest(30*time.Second, http.MethodGet, "organizations", nil, nil)
	if err != nil {
		return errors.New("failed to fetch the `organizationId`: " + err.Error())
	}
	marshal, e := io.ReadAll(response.Body)
	if e != nil {
		return e
	}

	result := make([]map[string]any, 0)
	if e = json.Unmarshal(marshal, &result); e != nil {
		return e
	}
	if uid, _ := result[0]["uuid"]; uid != nil && uid != "" {
		wc.organizationId = uid.(string)
		return nil
	}
	return errors.New("failed to fetch the `organizationId`")
}

func (wc *WebClaude2) createConversation() error {
	if wc.organizationId == "" {
		return errors.New("there is no corresponding `organizationId`")
	}

	headers := make(Kv)
	headers["user-agent"] = UA

	params := make(map[string]any)
	params["name"] = ""
	params["uuid"] = uuid.NewString()
	response, err := wc.newRequest(30*time.Second, http.MethodPost, "organizations/"+wc.organizationId+"/chat_conversations", headers, params)
	if err != nil {
		return errors.New("failed to fetch the `conversationId`: " + err.Error())
	}

	marshal, e := io.ReadAll(response.Body)
	if e != nil {
		return e
	}
	result := make(Kv, 0)
	if e = json.Unmarshal(marshal, &result); e != nil {
		return e
	}

	if uid, _ := result["uuid"]; uid != "" {
		wc.conversationId = uid
		return nil
	}
	return errors.New("failed to fetch the `conversationId`")
}

func (wc *WebClaude2) Delete() {
	if wc.organizationId == "" {
		return
	}
	if wc.conversationId == "" {
		return
	}
	_delMods()
	headers := make(Kv)
	headers["user-agent"] = UA
	_, _ = wc.newRequest(10*time.Second, http.MethodDelete, "organizations/"+wc.organizationId+"/chat_conversations/"+wc.conversationId, headers, nil)
}

func (wc *WebClaude2) PostMessage(timeout time.Duration, prompt string, attrs []types.Attachment) (*models.Response, error) {
	if wc.organizationId == "" {
		return nil, errors.New("there is no corresponding `organization-id`")
	}
	if wc.conversationId == "" {
		return nil, errors.New("there is no corresponding `conversation-id`")
	}

	params := make(map[string]any)
	if len(attrs) > 0 {
		params["attachments"] = attrs
	} else {
		params["attachments"] = []any{}
	}
	params["conversation_uuid"] = wc.conversationId
	params["organization_uuid"] = wc.organizationId
	params["text"] = prompt
	params["completion"] = Kv{
		"model":    wc.mod,
		"prompt":   prompt,
		"timezone": "America/New_York",
	}

	headers := make(Kv)
	headers["user-agent"] = UA
	headers["referer"] = "https://claude.ai"
	headers["accept"] = "text/event-stream"
	return wc.newRequest(timeout, http.MethodPost, "append_message", headers, params)
}

func (wc *WebClaude2) newRequest(timeout time.Duration, method string, route string, headers map[string]string, params map[string]any) (response *models.Response, err error) {
	if method == http.MethodGet {
		var search []string
		for key, value := range params {
			if v, ok := value.(string); ok {
				search = append(search, key+"="+v)
			}
		}

		if len(search) > 0 {
			route += "?" + strings.Join(search, "&")
		}

		params = nil
	}

	req := url.NewRequest()
	req.Timeout = timeout
	if method != http.MethodGet && params != nil {
		req.Json = params
	}

	if wc.Agency != "" {
		req.Proxies = wc.Agency
	}

	uHeaders := url.NewHeaders()
	for k, v := range headers {
		uHeaders.Set(k, v)
	}

	for k, v := range wc.Headers {
		uHeaders.Set(k, v)
	}

	req.Headers = uHeaders
	req.Ja3 = JA3

	bu := WebClaude2BU
	if wc.BaseURL != "" {
		bu = wc.BaseURL
	}

	if !strings.HasSuffix(bu, "/") {
		bu += "/"
	}

	switch method {
	case http.MethodGet:
		response, err = requests.Get(bu+route, req)
	default:
		response, err = requests.RequestStream(method, bu+route, req)
	}

	if err != nil {
		return nil, err
	}

	if response.StatusCode >= 400 {
		var data []byte
		data, err = io.ReadAll(response.Body)
		defer response.Body.Close()
		if err != nil {
			return nil, err
		}
		encoding := response.Headers.Get("Content-Encoding")
		requests.DecompressBody(&data, encoding)
		var wap types.ErrorWrapper
		err = json.Unmarshal(data, &wap)
		if err != nil {
			return nil, fmt.Errorf("%d: %s", response.StatusCode, data)
		}
		return nil, &wap
	}

	return response, nil
}

// ====

func CatchUnmarshal(data []byte, v any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if rec, ok := r.(string); ok {
				err = errors.New(rec)
			}
		}
	}()
	return json.Unmarshal(data, v)
}
