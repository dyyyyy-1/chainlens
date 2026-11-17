package decoder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// ErrNotVerified 表示该地址在 Sourcify 上没有已验证的源码。
var ErrNotVerified = errors.New("decoder: contract not verified on sourcify")

// sourcifyBaseURL 是 Sourcify v2 接口的基址。
const sourcifyBaseURL = "https://sourcify.dev/server/v2/contract"

// sourcifyMaxAttempts 是单次拉取的最大尝试次数，仅对网络错误与 5xx 重试。
const sourcifyMaxAttempts = 2

// SourcifyClient 从 Sourcify 拉取已验证合约的 ABI。
//
// 选 Sourcify 而不是 Etherscan：无需注册与 API key。HANDOFF 第 2 节要的
// 「失败就退化到原始日志层」策略不变，只是换了 ABI 供应商。
//
// 注意返回的 ABI 不一定属于目标地址本身：自己也是代理合约（例如 USDC 的
// FiatTokenProxy）时，里面只有 AdminChanged / Upgraded 两个事件，
// 真正的业务事件在实现合约上。调用方必须先做代理识别再按实现地址拉取。
type SourcifyClient struct {
	// BaseURL 允许测试注入本地端点；生产使用默认值。
	BaseURL string

	http *http.Client
}

// NewSourcifyClient 构造客户端，timeout 为单次 HTTP 请求的超时上限。
func NewSourcifyClient(timeout time.Duration) *SourcifyClient {
	return &SourcifyClient{
		BaseURL: sourcifyBaseURL,
		http:    &http.Client{Timeout: timeout},
	}
}

// FetchABI 返回合约的 ABI JSON 数组。
//
// 未验证时返回 ErrNotVerified；网络错误与 5xx 会重试一次，4xx 直接返回。
func (c *SourcifyClient) FetchABI(ctx context.Context, chainID uint64, addr common.Address) ([]byte, error) {
	endpoint := fmt.Sprintf("%s/%d/%s?fields=abi", strings.TrimRight(c.BaseURL, "/"), chainID, strings.ToLower(addr.Hex()))

	var lastErr error
	for attempt := 1; attempt <= sourcifyMaxAttempts; attempt++ {
		abiJSON, err := c.fetchOnce(ctx, endpoint, addr)
		if err == nil {
			return abiJSON, nil
		}
		if errors.Is(err, ErrNotVerified) || ctx.Err() != nil {
			return nil, err
		}
		lastErr = err

		if attempt < sourcifyMaxAttempts {
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, fmt.Errorf("decoder: fetch abi of %s: %w", addr.Hex(), ctx.Err())
			case <-timer.C:
			}
		}
	}

	return nil, lastErr
}

func (c *SourcifyClient) fetchOnce(ctx context.Context, endpoint string, addr common.Address) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("decoder: build sourcify request for %s: %w", addr.Hex(), err)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("decoder: fetch abi of %s: %w", addr.Hex(), err)
	}
	defer func() { _ = response.Body.Close() }()

	switch {
	case response.StatusCode == http.StatusNotFound:
		return nil, ErrNotVerified
	case response.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("decoder: sourcify returned %s for %s", response.Status, addr.Hex())
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("decoder: read sourcify response for %s: %w", addr.Hex(), err)
	}

	var payload struct {
		ABI json.RawMessage `json:"abi"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decoder: parse sourcify response for %s: %w", addr.Hex(), err)
	}
	if len(payload.ABI) == 0 || string(payload.ABI) == "null" {
		return nil, ErrNotVerified
	}

	return payload.ABI, nil
}
