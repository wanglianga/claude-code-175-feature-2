// Package migrationsfs 通过 go:embed 嵌入数据库迁移与 Swagger 静态资源。
package migrationsfs

import "embed"

//go:embed migrations/*.sql
var FS embed.FS

//go:embed web/openapi.yaml
var OpenAPIYAML []byte

//go:embed all:web/swagger-ui
var SwaggerUI embed.FS
