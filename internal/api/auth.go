package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// tokenPrefix 是 Authorization 头的方案前缀。
const tokenPrefix = "Bearer "

// authRequired 在配置了 API_TOKEN 时校验 Bearer token。
//
// token 为空表示不启用鉴权——本地开发与内网部署默认保持现在的行为，
// 不会因为升级代码就突然要求凭据。
//
// 这是**静态共享密钥**，不是完整的身份体系：它只能回答「调用方是否知道口令」，
// 不能区分调用者、不能撤销单个调用方、也不做细粒度授权。要这些能力需要真正的
// 认证服务，那超出本项目的范围，已在 README 的局限里写明。
func authRequired(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided, ok := bearerToken(r)
		// 定长比较：逐字节比较会在第一个不同字节处返回，泄漏前缀匹配长度。
		if !ok || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="chainlens"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
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
