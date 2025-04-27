// cmd/gateway/router/router.go
package router

import (
	"fmt"
	"net/http" // <-- Import os

	// <-- Import path/filepath
	"strings" // <-- Import strings

	"go-xlive/cmd/gateway/handler"    // 导入 handler 包
	"go-xlive/cmd/gateway/middleware" // 导入 middleware 包

	// 导入 db 包
	// !!! 替换模块路径 !!!

	eventv1 "go-xlive/gen/go/event/v1"
	realtimev1 "go-xlive/gen/go/realtime/v1"
	sessionv1 "go-xlive/gen/go/session/v1"

	"encoding/json" // <-- Add import
	"io/ioutil"     // <-- Add import (or "os" for Go 1.16+)
	"path/filepath" // <-- Add import

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"go.opentelemetry.io/otel"
	"go.uber.org/zap"
)

// mergeOpenAPISpecs reads all *.swagger.json files from the specified directory,
// merges their paths, definitions, and tags into a single OpenAPI spec.
func mergeOpenAPISpecs(logger *zap.Logger, dir string) (map[string]interface{}, error) {
	mergedSpec := map[string]interface{}{
		"swagger": "2.0",
		"info": map[string]interface{}{
			"title":       "Go-Xlive API Gateway",
			"version":     "1.0", // Or read from a base file/config
			"description": "Combined API documentation for all services.",
		},
		"schemes":     []string{"http", "https"}, // Or read from a base file/config
		"consumes":    []string{"application/json"},
		"produces":    []string{"application/json"},
		"paths":       map[string]interface{}{},
		"definitions": map[string]interface{}{},
		"tags":        []interface{}{},
		// Add other base fields like securityDefinitions if needed
	}
	paths := mergedSpec["paths"].(map[string]interface{})
	definitions := mergedSpec["definitions"].(map[string]interface{})
	tags := mergedSpec["tags"].([]interface{})
	seenTags := make(map[string]bool) // To deduplicate tags

	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read openapi directory %s: %w", dir, err)
	}

	for _, f := range files {
		if f.IsDir() {
			// Recursively search subdirectories (like service/v1)
			subDir := filepath.Join(dir, f.Name())
			subFiles, subErr := ioutil.ReadDir(subDir)
			if subErr != nil {
				logger.Warn("Failed to read openapi subdirectory", zap.String("subdir", subDir), zap.Error(subErr))
				continue
			}
			for _, sf := range subFiles {
				if sf.IsDir() {
					// Search one level deeper for version directories (like v1)
					versionDir := filepath.Join(subDir, sf.Name())
					versionFiles, versionErr := ioutil.ReadDir(versionDir)
					if versionErr != nil {
						logger.Warn("Failed to read openapi version directory", zap.String("versionDir", versionDir), zap.Error(versionErr))
						continue
					}
					for _, vf := range versionFiles {
						if !vf.IsDir() && strings.HasSuffix(vf.Name(), ".swagger.json") {
							filePath := filepath.Join(versionDir, vf.Name())
							logger.Debug("Merging OpenAPI spec file", zap.String("file", filePath))
							content, readErr := ioutil.ReadFile(filePath)
							if readErr != nil {
								logger.Warn("Failed to read spec file", zap.String("file", filePath), zap.Error(readErr))
								continue
							}

							var spec map[string]interface{}
							if jsonErr := json.Unmarshal(content, &spec); jsonErr != nil {
								logger.Warn("Failed to unmarshal spec file", zap.String("file", filePath), zap.Error(jsonErr))
								continue
							}

							// Merge paths
							if specPaths, ok := spec["paths"].(map[string]interface{}); ok {
								for path, pathItem := range specPaths {
									paths[path] = pathItem
								}
							}
							// Merge definitions
							if specDefs, ok := spec["definitions"].(map[string]interface{}); ok {
								for defName, defItem := range specDefs {
									definitions[defName] = defItem
								}
							}
							// Merge tags (deduplicated)
							if specTags, ok := spec["tags"].([]interface{}); ok {
								for _, tagItem := range specTags {
									if tagMap, ok := tagItem.(map[string]interface{}); ok {
										if tagName, ok := tagMap["name"].(string); ok {
											if !seenTags[tagName] {
												tags = append(tags, tagItem)
												seenTags[tagName] = true
											}
										}
									}
								}
							}
							// Optionally merge info fields if needed, handling conflicts
						}
					}
				}
			}
		}
	}
	mergedSpec["tags"] = tags // Update the tags slice in the map

	return mergedSpec, nil
}

// NewRouter 创建并配置网关的主 HTTP 路由器
func NewRouter(
	logger *zap.Logger,
	realtimeClient realtimev1.RealtimeServiceClient,
	sessionClient sessionv1.SessionServiceClient,
	eventClient eventv1.EventServiceClient,
	restMux *runtime.ServeMux,
	wsHub *handler.Hub,
) http.Handler {

	mainMux := http.NewServeMux()

	// --- WebSocket 路由 ---
	mainMux.HandleFunc("/ws/sessions/", func(w http.ResponseWriter, r *http.Request) {
		handler.ServeWs(wsHub, logger, realtimeClient, w, r)
	})

	// --- REST API 路由 (应用中间件) ---
	// 1. OTEL 中间件
	otelRestHandler := otelhttp.NewHandler(restMux, "api-gateway-rest",
		otelhttp.WithTracerProvider(otel.GetTracerProvider()),
		otelhttp.WithPropagators(otel.GetTextMapPropagator()),
	)
	// 2. 统一响应中间件
	unifiedHandler := middleware.UnifiedResponseMiddleware(otelRestHandler)

	// 3. 挂载到 /v1/ 前缀
	mainMux.Handle("/v1/", unifiedHandler)

	// --- 合并后的 OpenAPI JSON 路由 ---
	openAPIDir := "./gen/openapiv2"
	mainMux.HandleFunc("/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		mergedSpec, err := mergeOpenAPISpecs(logger.Named("openapi_merger"), openAPIDir)
		if err != nil {
			logger.Error("Failed to merge OpenAPI specs", zap.Error(err))
			http.Error(w, "Internal Server Error: Could not generate OpenAPI spec", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(mergedSpec); err != nil {
			logger.Error("Failed to encode merged OpenAPI spec", zap.Error(err))
		}
	})
	logger.Info("Serving merged OpenAPI spec at /openapi.json", zap.String("source_directory", openAPIDir))

	// --- Swagger UI 静态文件服务 ---
	swaggerUIDir := "./third_party/swagger-ui/"
	// StripPrefix 会移除 /swagger/ 前缀，然后 FileServer 会在 swaggerUIDir 中查找剩余路径的文件
	// 例如，访问 /swagger/index.html 会在 swaggerUIDir 中查找 index.html
	swaggerUIHandler := http.StripPrefix("/swagger/", http.FileServer(http.Dir(swaggerUIDir)))
	mainMux.Handle("/swagger/", swaggerUIHandler)
	logger.Info("Serving Swagger UI static files", zap.String("path", "/swagger/"), zap.String("directory", swaggerUIDir))

	return mainMux
}
