// Package web 内嵌看板的静态页面（开发文档 §7）：页面里没有数据，可以不登录访问；数据都从 /api/v1/* 取。
package web

import "embed"

// FS 包含页面、脚本、样式和第三方库。
//
//go:embed *.html assets vendor
var FS embed.FS
