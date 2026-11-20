package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
)

// tokenPrefix 是 Authorization 头的方案前缀。
const tokenPrefix = "Bearer "

// authRequired 在配置了口令时校验 Bearer token。
//
// tokens 为空表示不启用鉴权——本地开发与内网部署默认保持现在的行为，
// 不会因为升级代码就突然要求凭据。
//
// 支持多个口令是为了轮换：新旧口令可以短期并存，调用方分批切换，
// 不必在某一刻全员同步改配置。
//
// 这仍是**静态共享密钥**，不是完整的身份体系：它只能回答「调用方是否知道口令」，
// 不能区分调用者、不能撤销单个调用方、也不做细粒度授权。要这些能力需要真正的
// 认证服务，那超出本项目的范围，已在 README 的局限里写明。
func authRequired(tokens []string, next http.Handler) http.Handler {
	if len(tokens) == 0 {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided, ok := bearerToken(r)
		if !ok || !matchesAnyToken(provided, tokens) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="chainlens"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// matchesAnyToken 用定长比较逐个比对。
//
// 刻意**不短路**：一旦命中就 return，响应时间会随「命中第几个口令」变化，
// 足够让攻击者判断口令列表的顺序。列表只有几个元素，全量比较的代价可以忽略。
func matchesAnyToken(provided string, tokens []string) bool {
	matched := 0
	providedBytes := []byte(provided)
	for _, token := range tokens {
		matched |= subtle.ConstantTimeCompare(providedBytes, []byte(token))
	}
	return matched == 1
}

// bearerToken 取出 Authorization: Bearer <token> 中的 token。
//
// 方案名按 RFC 7235 大小写不敏感，因此用 EqualFold 比较；
// 但 token 本身区分大小写，不做任何规范化。
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if len(header) <= len(tokenPrefix) || !strings.EqualFold(header[:len(tokenPrefix)], tokenPrefix) {
		return "", false
	}

	token := strings.TrimSpace(header[len(tokenPrefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// tokenFingerprint 返回请求所带口令的短指纹（SHA-256 前 4 字节）。
//
// 目的是在访问日志里区分调用方，同时**不落明文口令**：日志常被集中收集，
// 明文密钥一旦进日志就等于泄露。指纹不可逆，也足够做「哪个调用方在打」的归因。
//
// 没有 Authorization 头时返回空串，调用方据此省略日志字段。
func tokenFingerprint(r *http.Request) string {
	token, ok := bearerToken(r)
	if !ok {
		return ""
	}

	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:4])
}
