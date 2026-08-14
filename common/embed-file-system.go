package common

import (
	"embed"
	"io/fs"
	"net/http"
	"os"
	"strings"

	"github.com/gin-contrib/static"
)

// Credit: https://github.com/gin-contrib/static/issues/19

type embedFileSystem struct {
	http.FileSystem
}

func (e *embedFileSystem) Exists(prefix string, path string) bool {
	_, err := e.Open(path)
	if err != nil {
		return false
	}
	return true
}

func (e *embedFileSystem) Open(name string) (http.File, error) {
	if name == "/" {
		// This will make sure the index page goes to NoRouter handler,
		// which will use the replaced index bytes with analytic codes.
		return nil, os.ErrNotExist
	}
	// embed FS（fs.Sub 的 ValidPath 校验）拒绝带尾斜杠的路径（如 /docs/），
	// 导致 static.Serve 对目录请求 Exists 判定失败、回落 NoRoute(SPA) 而非目录 index.html。
	// 去掉尾斜杠后交给 http.FS 打开目录，http.FileServer 会继续 serve 目录下的 index.html。
	name = strings.TrimSuffix(name, "/")
	if name == "" {
		name = "/"
	}
	return e.FileSystem.Open(name)
}

func EmbedFolder(fsEmbed embed.FS, targetPath string) static.ServeFileSystem {
	efs, err := fs.Sub(fsEmbed, targetPath)
	if err != nil {
		panic(err)
	}
	return &embedFileSystem{
		FileSystem: http.FS(efs),
	}
}
