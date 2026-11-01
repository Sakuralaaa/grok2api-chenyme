package console

import (
	"context"
	"strings"
)

// Console 允许的 x-cluster 只有 us-east-1、us-west-2、eu-west-1（2026-11-01 实测，
// 其它取值返回 400 "Cluster name not permitted."）。
//   - /responses 默认 us-east-1（与 Playground 一致，所有模型可用）。
//   - /images/* 固定 us-west-2：免费团队在 us-east-1 上图片接口 114/114 次返回
//     402 personal-team-blocked:spending-limit，us-west-2 约 20%~45% 成功；
//     不带 x-cluster 时会被随机分到东西两侧，成功率只有一半。
//   - eu-west-1 只提供 grok-4.3，且不支持图片/视频生成，不使用。
const (
	consoleClusterUSEast1 = "https://us-east-1.api.x.ai"
	consoleClusterUSWest2 = "https://us-west-2.api.x.ai"
)

type consoleClusterContextKey struct{}

// withConsoleCluster 为单次请求指定 x-cluster（用于生图工具链路跨集群降级）。
func withConsoleCluster(ctx context.Context, cluster string) context.Context {
	cluster = strings.TrimSpace(cluster)
	if cluster == "" {
		return ctx
	}
	return context.WithValue(ctx, consoleClusterContextKey{}, cluster)
}

func consoleClusterForRequest(ctx context.Context, path string) string {
	if ctx != nil {
		if cluster, ok := ctx.Value(consoleClusterContextKey{}).(string); ok && cluster != "" {
			return cluster
		}
	}
	switch {
	case strings.HasSuffix(path, "/responses"):
		return consoleClusterUSEast1
	case strings.Contains(path, "/images/"):
		return consoleClusterUSWest2
	default:
		return ""
	}
}
