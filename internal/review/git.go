// Package review contiene el motor de revisión de pull requests.
package review

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// cloneURL construye la URL de clonado inyectando un token de lectura solo en
// remotos http/https. Cualquier otro esquema (file://, ssh://, ...) se devuelve
// sin modificar.
func cloneURL(baseURL, repoFullName, token string) string {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return baseURL
	}

	u.User = url.UserPassword("x-access-token", token)

	parts := make([]string, 0, 2)
	if base := strings.Trim(u.Path, "/"); base != "" {
		parts = append(parts, base)
	}
	repo := strings.Trim(strings.TrimSuffix(strings.Trim(repoFullName, "/"), ".git"), "/")
	if repo != "" {
		parts = append(parts, repo)
	}
	u.Path = "/" + strings.Join(parts, "/") + ".git"
	u.RawPath = ""

	return u.String()
}

// git ejecuta un comando git en dir, honra ctx e incluye stderr en el error.
// op es una etiqueta fija: los argumentos no se incluyen para no filtrar el
// token del remote en mensajes de error o logs.
func git(ctx context.Context, dir, op string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			return "", fmt.Errorf("git %s: %w", op, err)
		}
		return "", fmt.Errorf("git %s: %w: %s", op, err, detail)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// fetchHeadRef devuelve el SHA de la línea index de FETCH_HEAD. El fetch de
// Clone pide primero baseRef y después headRef, así que 0 es la base y 1 la
// cabeza del PR.
func fetchHeadRef(dir string, index int) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, ".git", "FETCH_HEAD"))
	if err != nil {
		return "", fmt.Errorf("leer FETCH_HEAD: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) <= index {
		return "", fmt.Errorf("FETCH_HEAD: esperaba %d refs, obtuve %d", index+1, len(lines))
	}
	fields := strings.Fields(lines[index])
	if len(fields) == 0 {
		return "", fmt.Errorf("FETCH_HEAD: línea %d sin SHA", index)
	}
	return fields[0], nil
}

// fetchHeadBase devuelve el SHA del primer ref escrito en FETCH_HEAD (baseRef).
// OCR necesita un commit resuelto: el nombre de rama no existe como ref local en
// un clon shallow.
func fetchHeadBase(dir string) (string, error) {
	return fetchHeadRef(dir, 0)
}

// fetchHeadTip devuelve el SHA del segundo ref escrito en FETCH_HEAD, que es
// headRef (baseRef se pasa primero en el fetch).
func fetchHeadTip(dir string) (string, error) {
	return fetchHeadRef(dir, 1)
}

// Clone prepara un checkout efímero en dir con baseRef y headRef (en producción
// refs/pull/<n>/head) y devuelve el SHA del merge-base entre ambos. El fetch es
// shallow y se duplica la profundidad hasta encontrar el merge-base o agotar
// depthMax. El remote se elimina al terminar.
func Clone(ctx context.Context, dir, remoteURL, baseRef, headRef string, depthMax int) (mergeBase string, err error) {
	if depthMax < 1 {
		depthMax = 1
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("crear directorio de checkout: %w", err)
	}
	if _, err := git(ctx, dir, "init", "--no-pager", "init"); err != nil {
		return "", err
	}
	if _, err := git(ctx, dir, "remote add", "--no-pager", "remote", "add", "origin", remoteURL); err != nil {
		return "", err
	}

	for depth := 1; ; {
		if _, err := git(ctx, dir, "fetch", "--no-pager", "fetch", "--no-tags",
			fmt.Sprintf("--depth=%d", depth), "origin", baseRef, headRef); err != nil {
			return "", err
		}

		headSHA, err := fetchHeadTip(dir)
		if err != nil {
			return "", err
		}

		base, mbErr := git(ctx, dir, "merge-base", "--no-pager", "merge-base", "FETCH_HEAD", headSHA)
		if mbErr == nil && base != "" {
			if _, err := git(ctx, dir, "checkout", "--no-pager",
				"-c", "advice.detachedHead=false", "checkout", "--detach", headSHA); err != nil {
				return "", err
			}
			if _, err := git(ctx, dir, "remote remove", "--no-pager", "remote", "remove", "origin"); err != nil {
				return "", err
			}
			return base, nil
		}

		if depth >= depthMax {
			detail := "sin resultado"
			if mbErr != nil {
				detail = mbErr.Error()
			}
			return "", fmt.Errorf("merge-base no encontrado con profundidad máxima %d: %s", depthMax, detail)
		}
		depth *= 2
		if depth > depthMax {
			depth = depthMax
		}
	}
}
