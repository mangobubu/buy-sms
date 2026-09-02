package web

import "embed"

// Dist 包含生产环境中由前端构建得到的静态文件。
// 目录形式的模式会递归嵌入资源；placeholder.txt 让全新检出也能通过编译。
//
//go:embed dist
var Dist embed.FS
