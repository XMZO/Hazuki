package admin

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"strings"
)

//go:embed ui/templates/*.html ui/templates/pages/*.html ui/templates/partials/*.html ui/assets/*
var uiFS embed.FS

var uiAssetsFS = mustSub(uiFS, "ui/assets")

var bodyTemplateAllowList = map[string]struct{}{
	"login":         {},
	"setup":         {},
	"dashboard":     {},
	"system":        {},
	"redis_cache":   {},
	"traffic":       {},
	"wizard":        {},
	"torcherino":    {},
	"cdnjs":         {},
	"git":           {},
	"sakuya_oplist": {},
	"patchouli":     {},
	"versions":      {},
	"export":        {},
	"import":        {},
	"account":       {},
}

var pageTemplates = mustLoadTemplates()

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

func mustLoadTemplates() *template.Template {
	t := template.New("layout")
	t = t.Funcs(template.FuncMap{
		"render": func(name string, data any) (template.HTML, error) {
			if _, ok := bodyTemplateAllowList[name]; !ok {
				return "", fmt.Errorf("unknown template: %q", name)
			}

			var buf bytes.Buffer
			if err := t.ExecuteTemplate(&buf, name, data); err != nil {
				return "", err
			}
			return template.HTML(buf.String()), nil
		},
		"statusCtx": func(root any, status serviceStatus) any {
			return struct {
				Root   any
				Status serviceStatus
			}{
				Root:   root,
				Status: status,
			}
		},
		"dict": func(kv ...any) (map[string]any, error) {
			if len(kv)%2 != 0 {
				return nil, fmt.Errorf("dict: odd number of arguments")
			}
			m := make(map[string]any, len(kv)/2)
			for i := 0; i < len(kv); i += 2 {
				k, ok := kv[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: key %v is not a string", kv[i])
				}
				m[k] = kv[i+1]
			}
			return m, nil
		},
		"lower": strings.ToLower,
		"initial": func(s string) string {
			for _, r := range strings.TrimSpace(s) {
				return strings.ToUpper(string(r))
			}
			return "?"
		},
	})

	template.Must(t.ParseFS(
		uiFS,
		"ui/templates/*.html",
		"ui/templates/pages/*.html",
		"ui/templates/partials/*.html",
	))
	return t
}
