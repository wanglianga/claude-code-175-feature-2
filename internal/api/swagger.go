package api

import (
	"io/fs"
	"net/http"

	migrationsfs "deposit-escrow"
)

// mountSwagger 在路由表里登记占位; 实际静态服务在 main 中通过 http.Mux 挂载,
// 以支持 /swagger/ 前缀通配(自研路由只做定长匹配)。
func (s *Server) mountSwagger(r *Router) {
	r.Handle("GET", "swagger", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/swagger/", http.StatusMovedPermanently)
	})
}

// SwaggerHandler 返回 /swagger/ 下的静态文件 handler(已剥离前缀)。
func SwaggerHandler() http.Handler {
	sub, err := fs.Sub(migrationsfs.SwaggerUI, "web/swagger-ui")
	if err != nil {
		panic(err)
	}
	return http.StripPrefix("/swagger/", http.FileServer(http.FS(sub)))
}

// OpenAPIHandler 返回 openapi.yaml 原文。
func OpenAPIHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		_, _ = w.Write(migrationsfs.OpenAPIYAML)
	})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	status := map[string]string{"postgres": "ok", "redis": "ok"}
	code := 200
	if err := s.svc.PingDB(r.Context()); err != nil {
		status["postgres"] = err.Error()
		code = 503
	}
	if err := s.rdb.Ping(r.Context()).Err(); err != nil {
		status["redis"] = err.Error()
		code = 503
	}
	status["kafka"] = map[bool]string{true: "on", false: "disabled"}[s.svc.KafkaOn()]
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	writeJSON(w, code, status)
}
