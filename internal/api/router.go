// Package api 是 HTTP 接口层。Go 1.19 的 ServeMux 不支持方法/路径参数,
// 这里用一个极小的自研路由, 避免引入额外 Web 框架。
package api

import (
	"context"
	"net/http"
	"strings"
)

type ctxKey int

const paramsKey ctxKey = 1

type route struct {
	method   string
	segments []string
	handler  http.HandlerFunc
}

type Router struct {
	routes   []route
	notFound http.HandlerFunc
}

func NewRouter() *Router { return &Router{} }

func (r *Router) Handle(method, pattern string, h http.HandlerFunc) {
	r.routes = append(r.routes, route{method: method, segments: splitPattern(pattern), handler: h})
}

func splitPattern(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	path := strings.Trim(req.URL.Path, "/")
	var parts []string
	if path != "" {
		parts = strings.Split(path, "/")
	}
	for _, rt := range r.routes {
		if rt.method != req.Method || len(rt.segments) != len(parts) {
			continue
		}
		params := map[string]string{}
		match := true
		for i, seg := range rt.segments {
			if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
				params[strings.TrimSuffix(strings.TrimPrefix(seg, "{"), "}")] = parts[i]
			} else if seg != parts[i] {
				match = false
				break
			}
		}
		if match {
			ctx := context.WithValue(req.Context(), paramsKey, params)
			rt.handler(w, req.WithContext(ctx))
			return
		}
	}
	if r.notFound != nil {
		r.notFound(w, req)
		return
	}
	http.NotFound(w, req)
}

func Param(req *http.Request, name string) string {
	if m, ok := req.Context().Value(paramsKey).(map[string]string); ok {
		return m[name]
	}
	return ""
}
