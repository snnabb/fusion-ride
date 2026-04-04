package web

import (
	"embed"
	"io/fs"
)

//go:embed static/*
var staticFiles embed.FS

// StaticFS 返回嵌入的静态文件系统。
func StaticFS() fs.FS {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
