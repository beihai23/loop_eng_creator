package skill

import (
	"bytes"
	"text/template"
)

func render(tmpl string, input any) (string, error) {
	t, err := template.New("s").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, input); err != nil {
		return "", err
	}
	return buf.String(), nil
}
