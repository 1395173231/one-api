package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"one-api/common"
	"one-api/model"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	APITypeOpenAI = iota
	APITypeClaude
	APITypePaLM
	APITypeBaidu
	APITypeZhipu
	APITypeAli
	APITypeXunfei
	APITypeAIProxyLibrary
	APITypeTencent
	APITypeOpenAIWeb
)

var httpClient *http.Client
var timeoutHTTPClient *http.Client
var impatientHTTPClient *http.Client

func init() {
	if common.RelayTimeout == 0 {
		httpClient = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}
	} else {
		httpClient = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
			Timeout: time.Duration(common.RelayTimeout) * time.Second,
		}
	}
	timeout := common.RelayResponseTimeout
	if timeout < 5 {
		timeout = 5
	}
	timeoutHTTPClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
			ResponseHeaderTimeout: time.Second * time.Duration(timeout),
			MaxIdleConnsPerHost:   100,
		},
	}

	impatientHTTPClient = &http.Client{
		Timeout: 5 * time.Second,
	}
}

func relayTextHelper(c *gin.Context, relayMode int) *OpenAIErrorWithStatusCode {
	channelType := c.GetInt("channel")
	channelId := c.GetInt("channel_id")
	tokenId := c.GetInt("token_id")
	userId := c.GetInt("id")
	group := c.GetString("group")
	var textRequest GeneralOpenAIRequest
	err := common.UnmarshalBodyReusable(c, &textRequest)
	if err != nil {
		return errorWrapper(err, "bind_request_body_failed", http.StatusBadRequest)
	}
	if relayMode == RelayModeModerations && textRequest.Model == "" {
		textRequest.Model = "text-moderation-latest"
	}
	if relayMode == RelayModeEmbeddings && textRequest.Model == "" {
		textRequest.Model = c.Param("model")
	}
	// request validation
	if textRequest.Model == "" {
		return errorWrapper(errors.New("model is required"), "required_field_missing", http.StatusBadRequest)
	}
	switch relayMode {
	case RelayModeCompletions:
		if textRequest.Prompt == "" {
			return errorWrapper(errors.New("field prompt is required"), "required_field_missing", http.StatusBadRequest)
		}
	case RelayModeChatCompletions:
		if textRequest.Messages == nil || len(textRequest.Messages) == 0 {
			return errorWrapper(errors.New("field messages is required"), "required_field_missing", http.StatusBadRequest)
		}
	case RelayModeEmbeddings:
	case RelayModeModerations:
		if textRequest.Input == "" {
			return errorWrapper(errors.New("field input is required"), "required_field_missing", http.StatusBadRequest)
		}
	case RelayModeEdits:
		if textRequest.Instruction == "" {
			return errorWrapper(errors.New("field instruction is required"), "required_field_missing", http.StatusBadRequest)
		}
	}
	// map model name
	modelMapping := c.GetString("model_mapping")
	isModelMapped := false
	if modelMapping != "" && modelMapping != "{}" {
		modelMap := make(map[string]string)
		err := json.Unmarshal([]byte(modelMapping), &modelMap)
		if err != nil {
			return errorWrapper(err, "unmarshal_model_mapping_failed", http.StatusInternalServerError)
		}
		if modelMap[textRequest.Model] != "" {
			textRequest.Model = modelMap[textRequest.Model]
			isModelMapped = true
		}
	}
	apiType := APITypeOpenAI
	switch channelType {
	case common.ChannelTypeAnthropic:
		apiType = APITypeClaude
	case common.ChannelTypeBaidu:
		apiType = APITypeBaidu
	case common.ChannelTypePaLM:
		apiType = APITypePaLM
	case common.ChannelTypeZhipu:
		apiType = APITypeZhipu
	case common.ChannelTypeAli:
		apiType = APITypeAli
	case common.ChannelTypeXunfei:
		apiType = APITypeXunfei
	case common.ChannelTypeAIProxyLibrary:
		apiType = APITypeAIProxyLibrary
	case common.ChannelTypeTencent:
		apiType = APITypeTencent
	case common.ChannelTypeOpenAIWeb:
		apiType = APITypeOpenAIWeb
	}
	baseURL := common.ChannelBaseURLs[channelType]
	requestURL := c.Request.URL.String()
	if c.GetString("base_url") != "" {
		baseURL = c.GetString("base_url")
	}
	fullRequestURL := getFullRequestURL(baseURL, requestURL, channelType)
	if channelType == common.ChannelTypeOpenAI {
		if strings.HasPrefix(baseURL, "https://gateway.ai.cloudflare.com") {
			fullRequestURL = fmt.Sprintf("%s%s", baseURL, strings.TrimPrefix(requestURL, "/v1"))
		}
	}
	if c.GetString("full_url") != "" && strings.HasSuffix(requestURL, "chat/completions") {
		fullRequestURL = c.GetString("full_url")
	}
	switch apiType {
	case APITypeOpenAI:
		if channelType == common.ChannelTypeAzure {
			// https://learn.microsoft.com/en-us/azure/cognitive-services/openai/chatgpt-quickstart?pivots=rest-api&tabs=command-line#rest-api
			apiVersion := GetAPIVersion(c)
			requestURL := strings.Split(requestURL, "?")[0]
			requestURL = fmt.Sprintf("%s?api-version=%s", requestURL, apiVersion)
			baseURL = c.GetString("base_url")
			task := strings.TrimPrefix(requestURL, "/v1/")
			model_ := textRequest.Model
			model_ = strings.Replace(model_, ".", "", -1)
			// https://github.com/songquanpeng/one-api/issues/67
			model_ = strings.TrimSuffix(model_, "-0301")
			model_ = strings.TrimSuffix(model_, "-0314")
			model_ = strings.TrimSuffix(model_, "-0613")

			requestURL = fmt.Sprintf("/openai/deployments/%s/%s", model_, task)
			fullRequestURL = getFullRequestURL(baseURL, requestURL, channelType)
		}
	case APITypeClaude:
		fullRequestURL = "https://api.anthropic.com/v1/complete"
		if baseURL != "" {
			fullRequestURL = fmt.Sprintf("%s/v1/complete", baseURL)
		}
	case APITypeBaidu:
		switch textRequest.Model {
		case "ERNIE-Bot":
			fullRequestURL = "https://aip.baidubce.com/rpc/2.0/ai_custom/v1/wenxinworkshop/chat/completions"
		case "ERNIE-Bot-turbo":
			fullRequestURL = "https://aip.baidubce.com/rpc/2.0/ai_custom/v1/wenxinworkshop/chat/eb-instant"
		case "ERNIE-Bot-4":
			fullRequestURL = "https://aip.baidubce.com/rpc/2.0/ai_custom/v1/wenxinworkshop/chat/completions_pro"
		case "BLOOMZ-7B":
			fullRequestURL = "https://aip.baidubce.com/rpc/2.0/ai_custom/v1/wenxinworkshop/chat/bloomz_7b1"
		case "Embedding-V1":
			fullRequestURL = "https://aip.baidubce.com/rpc/2.0/ai_custom/v1/wenxinworkshop/embeddings/embedding-v1"
		}
		apiKey := c.Request.Header.Get("Authorization")
		apiKey = strings.TrimPrefix(apiKey, "Bearer ")
		var err error
		if apiKey, err = getBaiduAccessToken(apiKey); err != nil {
			return errorWrapper(err, "invalid_baidu_config", http.StatusInternalServerError)
		}
		fullRequestURL += "?access_token=" + apiKey
	case APITypePaLM:
		fullRequestURL = "https://generativelanguage.googleapis.com/v1beta2/models/chat-bison-001:generateMessage"
		if baseURL != "" {
			fullRequestURL = fmt.Sprintf("%s/v1beta2/models/chat-bison-001:generateMessage", baseURL)
		}
		apiKey := c.Request.Header.Get("Authorization")
		apiKey = strings.TrimPrefix(apiKey, "Bearer ")
		fullRequestURL += "?key=" + apiKey
	case APITypeZhipu:
		method := "invoke"
		if textRequest.Stream {
			method = "sse-invoke"
		}
		fullRequestURL = fmt.Sprintf("https://open.bigmodel.cn/api/paas/v3/model-api/%s/%s", textRequest.Model, method)
	case APITypeAli:
		fullRequestURL = "https://dashscope.aliyuncs.com/api/v1/services/aigc/text-generation/generation"
		if relayMode == RelayModeEmbeddings {
			fullRequestURL = "https://dashscope.aliyuncs.com/api/v1/services/embeddings/text-embedding/text-embedding"
		}
	case APITypeTencent:
		fullRequestURL = "https://hunyuan.cloud.tencent.com/hyllm/v1/chat/completions"
	case APITypeAIProxyLibrary:
		fullRequestURL = fmt.Sprintf("%s/api/library/ask", baseURL)
	case APITypeOpenAIWeb:
		fullRequestURL = fmt.Sprintf("%s/backend-api/conversation", baseURL)
	}
	var promptTokens int
	var completionTokens int
	switch relayMode {
	case RelayModeChatCompletions:
		promptTokens = countTokenMessages(textRequest.Messages, textRequest.Model)
	case RelayModeCompletions:
		promptTokens = countTokenInput(textRequest.Prompt, textRequest.Model)
	case RelayModeModerations:
		promptTokens = countTokenInput(textRequest.Input, textRequest.Model)
	}
	preConsumedTokens := common.PreConsumedQuota
	if textRequest.MaxTokens != 0 {
		preConsumedTokens = promptTokens + textRequest.MaxTokens
	}
	modelRatio := common.GetModelRatio(textRequest.Model)
	groupRatio := common.GetGroupRatio(group)
	ratio := modelRatio * groupRatio
	preConsumedQuota := int(float64(preConsumedTokens) * ratio)
	userQuota, err := model.CacheGetUserQuota(userId)
	if err != nil {
		return errorWrapper(err, "get_user_quota_failed", http.StatusInternalServerError)
	}
	if userQuota-preConsumedQuota < 0 {
		return errorWrapper(errors.New("user quota is not enough"), "insufficient_user_quota", http.StatusForbidden)
	}
	err = model.CacheDecreaseUserQuota(userId, preConsumedQuota)
	if err != nil {
		return errorWrapper(err, "decrease_user_quota_failed", http.StatusInternalServerError)
	}
	if userQuota > 100*preConsumedQuota {
		// in this case, we do not pre-consume quota
		// because the user has enough quota
		preConsumedQuota = 0
		common.LogInfo(c.Request.Context(), fmt.Sprintf("user %d has enough quota %d, trusted and no need to pre-consume", userId, userQuota))
	}
	if preConsumedQuota > 0 {
		err := model.PreConsumeTokenQuota(tokenId, preConsumedQuota)
		if err != nil {
			return errorWrapper(err, "pre_consume_token_quota_failed", http.StatusForbidden)
		}
	}
	var requestBody io.Reader
	if isModelMapped {
		jsonStr, err := json.Marshal(textRequest)
		if err != nil {
			return errorWrapper(err, "marshal_text_request_failed", http.StatusInternalServerError)
		}
		requestBody = bytes.NewBuffer(jsonStr)
	} else {
		requestBody = c.Request.Body
	}
	switch apiType {
	case APITypeClaude:
		claudeRequest := requestOpenAI2Claude(textRequest)
		jsonStr, err := json.Marshal(claudeRequest)
		if err != nil {
			return errorWrapper(err, "marshal_text_request_failed", http.StatusInternalServerError)
		}
		requestBody = bytes.NewBuffer(jsonStr)
	case APITypeBaidu:
		var jsonData []byte
		var err error
		switch relayMode {
		case RelayModeEmbeddings:
			baiduEmbeddingRequest := embeddingRequestOpenAI2Baidu(textRequest)
			jsonData, err = json.Marshal(baiduEmbeddingRequest)
		default:
			baiduRequest := requestOpenAI2Baidu(textRequest)
			jsonData, err = json.Marshal(baiduRequest)
		}
		if err != nil {
			return errorWrapper(err, "marshal_text_request_failed", http.StatusInternalServerError)
		}
		requestBody = bytes.NewBuffer(jsonData)
	case APITypePaLM:
		palmRequest := requestOpenAI2PaLM(textRequest)
		jsonStr, err := json.Marshal(palmRequest)
		if err != nil {
			return errorWrapper(err, "marshal_text_request_failed", http.StatusInternalServerError)
		}
		requestBody = bytes.NewBuffer(jsonStr)
	case APITypeZhipu:
		zhipuRequest := requestOpenAI2Zhipu(textRequest)
		jsonStr, err := json.Marshal(zhipuRequest)
		if err != nil {
			return errorWrapper(err, "marshal_text_request_failed", http.StatusInternalServerError)
		}
		requestBody = bytes.NewBuffer(jsonStr)
	case APITypeAli:
		var jsonStr []byte
		var err error
		switch relayMode {
		case RelayModeEmbeddings:
			aliEmbeddingRequest := embeddingRequestOpenAI2Ali(textRequest)
			jsonStr, err = json.Marshal(aliEmbeddingRequest)
		default:
			aliRequest := requestOpenAI2Ali(textRequest)
			jsonStr, err = json.Marshal(aliRequest)
		}
		if err != nil {
			return errorWrapper(err, "marshal_text_request_failed", http.StatusInternalServerError)
		}
		requestBody = bytes.NewBuffer(jsonStr)
	case APITypeTencent:
		apiKey := c.Request.Header.Get("Authorization")
		apiKey = strings.TrimPrefix(apiKey, "Bearer ")
		appId, secretId, secretKey, err := parseTencentConfig(apiKey)
		if err != nil {
			return errorWrapper(err, "invalid_tencent_config", http.StatusInternalServerError)
		}
		tencentRequest := requestOpenAI2Tencent(textRequest)
		tencentRequest.AppId = appId
		tencentRequest.SecretId = secretId
		jsonStr, err := json.Marshal(tencentRequest)
		if err != nil {
			return errorWrapper(err, "marshal_text_request_failed", http.StatusInternalServerError)
		}
		sign := getTencentSign(*tencentRequest, secretKey)
		c.Request.Header.Set("Authorization", sign)
		requestBody = bytes.NewBuffer(jsonStr)
	case APITypeAIProxyLibrary:
		aiProxyLibraryRequest := requestOpenAI2AIProxyLibrary(textRequest)
		aiProxyLibraryRequest.LibraryId = c.GetString("library_id")
		jsonStr, err := json.Marshal(aiProxyLibraryRequest)
		if err != nil {
			return errorWrapper(err, "marshal_text_request_failed", http.StatusInternalServerError)
		}
		requestBody = bytes.NewBuffer(jsonStr)
	case APITypeOpenAIWeb:
		openAIWebRequest, _ := convertAPIRequest(textRequest)
		jsonStr, err := json.Marshal(openAIWebRequest)
		if err != nil {
			return errorWrapper(err, "marshal_text_request_failed", http.StatusInternalServerError)
		}
		requestBody = bytes.NewBuffer(jsonStr)
	}
	var req *http.Request
	var resp *http.Response
	isStream := textRequest.Stream

	if apiType != APITypeXunfei { // cause xunfei use websocket
		req, err = http.NewRequest(c.Request.Method, fullRequestURL, requestBody)
		if err != nil {
			return errorWrapper(err, "new_request_failed", http.StatusInternalServerError)
		}
		req.Header = c.Request.Header.Clone()
		apiKey := c.Request.Header.Get("Authorization")
		apiKey = strings.TrimPrefix(apiKey, "Bearer ")
		switch apiType {
		case APITypeOpenAI:
			if channelType == common.ChannelTypeAzure {
				req.Header.Set("api-key", apiKey)
			} else {
				req.Header.Set("Authorization", c.Request.Header.Get("Authorization"))
				if channelType == common.ChannelTypeOpenRouter {
					req.Header.Set("HTTP-Referer", "https://github.com/songquanpeng/one-api")
					req.Header.Set("X-Title", "One API")
				}
			}
		case APITypeClaude:
			req.Header.Set("x-api-key", apiKey)
			anthropicVersion := c.Request.Header.Get("anthropic-version")
			if anthropicVersion == "" {
				anthropicVersion = "2023-06-01"
			}
			req.Header.Set("anthropic-version", anthropicVersion)
		case APITypeZhipu:
			token := getZhipuToken(apiKey)
			req.Header.Set("Authorization", token)
		case APITypeAli:
			req.Header.Set("Authorization", "Bearer "+apiKey)
			if textRequest.Stream {
				req.Header.Set("X-DashScope-SSE", "enable")
			}
		case APITypeTencent:
			req.Header.Set("Authorization", apiKey)
		case APITypePaLM:
			// do not set Authorization header
		case APITypeOpenAIWeb:
			req, err = http.NewRequest(c.Request.Method, fullRequestURL, requestBody)
			randomIp, _ := randomIPFromRanges(IPRanges)
			randomIpString := randomIp.String()
			req.Header.Set("x-real-ip", randomIpString)
			req.Header.Set("x-forwarded-for", "2a06:98c0:3600::103, 2a0e:97c0:7d4:ff31::a,5.253.36.142,"+randomIpString)
			req.Header.Set("remote-host", randomIpString)
			req.Header.Set("Authorization", "Bearer "+apiKey)
		default:
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		if strings.Contains(apiKey, "ignore") {
			req.Header.Del("Authorization")
		}
		req.Header.Set("Content-Type", c.Request.Header.Get("Content-Type"))
		req.Header.Set("Accept", c.Request.Header.Get("Accept"))
		if isStream && c.Request.Header.Get("Accept") == "" {
			req.Header.Set("Accept", "text/event-stream")
		}
		//req.Header.Set("Connection", c.Request.Header.Get("Connection"))

		asyncNum := c.GetInt("async_num")
		if apiType == APITypeOpenAIWeb {
			resp, err = asyncHTTPDoWithOpenaiWeb(req, isStream)
		} else {
			if isStream {
				resp, err = asyncHTTPDo(req, asyncNum)
			} else {
				resp, err = httpClient.Do(req)
			}
		}

		if err != nil {
			return errorWrapper(err, "do_request_failed", http.StatusInternalServerError)
		}
		_ = req.Body.Close()
		_ = c.Request.Body.Close()
		isStream = isStream || strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")

		if resp.StatusCode != http.StatusOK {
			if preConsumedQuota != 0 {
				go func(ctx context.Context) {
					// return pre-consumed quota
					err = model.PostConsumeTokenQuota(tokenId, userQuota, -preConsumedQuota, preConsumedQuota, true)
					if err != nil {
						common.LogError(ctx, "error return pre-consumed quota: "+err.Error())
					}
				}(c.Request.Context())
			}
			return relayErrorHandler(resp)
		}
	}

	var textResponse TextResponse
	tokenName := c.GetString("token_name")

	defer func(ctx context.Context) {
		// c.Writer.Flush()
		go func() {
			quota := 0
			completionRatio := common.GetCompletionRatio(textRequest.Model)
			promptTokens = textResponse.Usage.PromptTokens
			completionTokens = textResponse.Usage.CompletionTokens
			quota = int(math.Ceil((float64(promptTokens) + float64(completionTokens)*completionRatio) * ratio))
			if ratio != 0 && quota <= 0 {
				quota = 1
			}
			totalTokens := promptTokens + completionTokens
			if totalTokens == 0 {
				// in this case, must be some error happened
				// we cannot just return, because we may have to return the pre-consumed quota
				quota = 0
			}
			quotaDelta := quota - preConsumedQuota
			err = model.PostConsumeTokenQuota(tokenId, userQuota, quotaDelta, preConsumedQuota, true)
			if err != nil {
				common.LogError(ctx, "error consuming token remain quota: "+err.Error())
			}
			err = model.CacheUpdateUserQuota(userId)
			if err != nil {
				common.LogError(ctx, "error update user quota cache: "+err.Error())
			}
			if quota != 0 {
				logContent := fmt.Sprintf("模型倍率 %.2f，分组倍率 %.2f", modelRatio, groupRatio)
				model.RecordConsumeLog(ctx, userId, channelId, promptTokens, completionTokens, textRequest.Model, tokenName, quota, logContent, tokenId, userQuota)
				model.UpdateUserUsedQuotaAndRequestCount(userId, quota)
				model.UpdateChannelUsedQuota(channelId, quota)
			}

		}()
	}(c.Request.Context())
	switch apiType {
	case APITypeOpenAIWeb:
		fallthrough
	case APITypeOpenAI:
		if isStream {
			err, responseText := openaiStreamHandler(c, resp, relayMode)
			if err != nil {
				return err
			}
			textResponse.Usage.PromptTokens = promptTokens
			textResponse.Usage.CompletionTokens = countTokenText(responseText, textRequest.Model)
			return nil
		} else {
			err, usage := openaiHandler(c, resp, promptTokens, textRequest.Model)
			if err != nil {
				return err
			}
			if usage != nil {
				textResponse.Usage = *usage
			}
			return nil
		}
	case APITypeClaude:
		if isStream {
			err, responseText := claudeStreamHandler(c, resp)
			if err != nil {
				return err
			}
			textResponse.Usage.PromptTokens = promptTokens
			textResponse.Usage.CompletionTokens = countTokenText(responseText, textRequest.Model)
			return nil
		} else {
			err, usage := claudeHandler(c, resp, promptTokens, textRequest.Model)
			if err != nil {
				return err
			}
			if usage != nil {
				textResponse.Usage = *usage
			}
			return nil
		}
	case APITypeBaidu:
		if isStream {
			err, usage := baiduStreamHandler(c, resp)
			if err != nil {
				return err
			}
			if usage != nil {
				textResponse.Usage = *usage
			}
			return nil
		} else {
			var err *OpenAIErrorWithStatusCode
			var usage *Usage
			switch relayMode {
			case RelayModeEmbeddings:
				err, usage = baiduEmbeddingHandler(c, resp)
			default:
				err, usage = baiduHandler(c, resp)
			}
			if err != nil {
				return err
			}
			if usage != nil {
				textResponse.Usage = *usage
			}
			return nil
		}
	case APITypePaLM:
		if textRequest.Stream { // PaLM2 API does not support stream
			err, responseText := palmStreamHandler(c, resp)
			if err != nil {
				return err
			}
			textResponse.Usage.PromptTokens = promptTokens
			textResponse.Usage.CompletionTokens = countTokenText(responseText, textRequest.Model)
			return nil
		} else {
			err, usage := palmHandler(c, resp, promptTokens, textRequest.Model)
			if err != nil {
				return err
			}
			if usage != nil {
				textResponse.Usage = *usage
			}
			return nil
		}
	case APITypeZhipu:
		if isStream {
			err, usage := zhipuStreamHandler(c, resp)
			if err != nil {
				return err
			}
			if usage != nil {
				textResponse.Usage = *usage
			}
			// zhipu's API does not return prompt tokens & completion tokens
			textResponse.Usage.PromptTokens = textResponse.Usage.TotalTokens
			return nil
		} else {
			err, usage := zhipuHandler(c, resp)
			if err != nil {
				return err
			}
			if usage != nil {
				textResponse.Usage = *usage
			}
			// zhipu's API does not return prompt tokens & completion tokens
			textResponse.Usage.PromptTokens = textResponse.Usage.TotalTokens
			return nil
		}
	case APITypeAli:
		if isStream {
			err, usage := aliStreamHandler(c, resp)
			if err != nil {
				return err
			}
			if usage != nil {
				textResponse.Usage = *usage
			}
			return nil
		} else {
			var err *OpenAIErrorWithStatusCode
			var usage *Usage
			switch relayMode {
			case RelayModeEmbeddings:
				err, usage = aliEmbeddingHandler(c, resp)
			default:
				err, usage = aliHandler(c, resp)
			}
			if err != nil {
				return err
			}
			if usage != nil {
				textResponse.Usage = *usage
			}
			return nil
		}
	case APITypeXunfei:
		auth := c.Request.Header.Get("Authorization")
		auth = strings.TrimPrefix(auth, "Bearer ")
		splits := strings.Split(auth, "|")
		if len(splits) != 3 {
			return errorWrapper(errors.New("invalid auth"), "invalid_auth", http.StatusBadRequest)
		}
		var err *OpenAIErrorWithStatusCode
		var usage *Usage
		if isStream {
			err, usage = xunfeiStreamHandler(c, textRequest, splits[0], splits[1], splits[2])
		} else {
			err, usage = xunfeiHandler(c, textRequest, splits[0], splits[1], splits[2])
		}
		if err != nil {
			return err
		}
		if usage != nil {
			textResponse.Usage = *usage
		}
		return nil
	case APITypeAIProxyLibrary:
		if isStream {
			err, usage := aiProxyLibraryStreamHandler(c, resp)
			if err != nil {
				return err
			}
			if usage != nil {
				textResponse.Usage = *usage
			}
			return nil
		} else {
			err, usage := aiProxyLibraryHandler(c, resp)
			if err != nil {
				return err
			}
			if usage != nil {
				textResponse.Usage = *usage
			}
			return nil
		}
	case APITypeTencent:
		if isStream {
			err, responseText := tencentStreamHandler(c, resp)
			if err != nil {
				return err
			}
			textResponse.Usage.PromptTokens = promptTokens
			textResponse.Usage.CompletionTokens = countTokenText(responseText, textRequest.Model)
			return nil
		} else {
			err, usage := tencentHandler(c, resp)
			if err != nil {
				return err
			}
			if usage != nil {
				textResponse.Usage = *usage
			}
			return nil
		}

	default:
		return errorWrapper(errors.New("unknown api type"), "unknown_api_type", http.StatusInternalServerError)
	}
}

func asyncHTTPDo(req *http.Request, asyncNum int) (*http.Response, error) {
	respCh := make(chan *http.Response)
	errCh := make(chan error)
	wg := &sync.WaitGroup{}
	cancelFuncs := make(map[int]context.CancelFunc)
	reqs := make([]*http.Request, asyncNum)
	resps := make([]*http.Response, 0)
	done := make(chan bool)
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	if asyncNum == 1 {
		reqs[0] = req
		reqs[0].Body = io.NopCloser(bytes.NewReader(bodyBytes))
		reqs[0].ContentLength = int64(len(bodyBytes))
	} else {
		for i := 0; i < asyncNum; i++ {
			reqs[i] = req.Clone(req.Context())
			reqs[i].Body = io.NopCloser(bytes.NewReader(bodyBytes))
			reqs[i].ContentLength = int64(len(bodyBytes))
		}
	}
	timer := time.NewTimer(5 * time.Second)

	var lastErr error
	var mux sync.Mutex

	for i, req := range reqs {
		wg.Add(1)
		ctx, cancel := context.WithCancel(context.Background())
		mux.Lock()
		cancelFuncs[i] = cancel
		mux.Unlock()
		go func(i int, req *http.Request, cancel context.CancelFunc) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					fmt.Println("Recovered from panic:", r)
				}
			}()
			req = req.WithContext(ctx)
			defer req.Body.Close()
			resp, err := timeoutHTTPClient.Do(req)
			if err != nil {
				mux.Lock()
				lastErr = err
				mux.Unlock()
				cancel()
				return
			}
			if resp.StatusCode == 200 {
				mux.Lock()
				delete(cancelFuncs, i)
				mux.Unlock()
			}
			respCh <- resp
		}(i, req, cancel)
	}

	go func() {
		wg.Wait()
		timer.Stop()
		close(done)
		close(respCh)
		close(errCh)
	}()

	defer func() {
		for _, res := range resps {
			if res != nil && res.Body != nil {
				_ = res.Body.Close()
			}
		}
	}()

	for {
		select {
		case resp, ok := <-respCh:
			if ok {
				if resp.StatusCode == 200 {
					timer.Stop()
					mux.Lock()
					for _, cancel := range cancelFuncs {
						cancel()
					}
					mux.Unlock()
					return resp, nil
				}
				resps = append(resps, resp)
			}
		case <-timer.C:
			if asyncNum > 1 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() {
						if r := recover(); r != nil {
							fmt.Println("Recovered from panic:", r)
						}
					}()
					resp, err := timeoutHTTPClient.Do(req)
					if err != nil {
						mux.Lock()
						lastErr = err
						mux.Unlock()
						return
					}
					respCh <- resp
				}()
			}
		case <-done:
			mux.Lock()
			for _, cancel := range cancelFuncs {
				cancel()
			}
			mux.Unlock()
			var resp *http.Response
			if len(resps) > 0 {
				resp = resps[0]
				resps = resps[1:]
			}
			return resp, lastErr
		}
	}
}
