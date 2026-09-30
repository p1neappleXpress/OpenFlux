package mobile

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/p1neappleXpress/OpenFlux/provision/phphost"
)

// Putting the PHP exit on a free web host, for the apps' "own node without a
// server" wizard. Every step is one call; what each does and answers is
// documented on phphost.Call (provision/phphost/api.go). Decisions live in the
// core and answers carry codes (phphost.Code*) for the app to word.

var php struct {
	mu       sync.Mutex
	progress []string
	cancel   context.CancelFunc
}

// PhpCall runs one step: method is "probe", "deploy", "remove", "check",
// "start", "stop", "node", "newRoom" or "link"; paramsJSON is phphost.Params
// as JSON (ftp, token, url, carrier, target, name, chain, waitSec). It blocks
// (a deploy uploads about 2 MB, a start waits for the node): call it from a
// worker thread. The answer is JSON: {"ok":true,"data":...} or {"ok":false,
// "error":...,"code":...,"param":...}. A deploy's progress is read with PhpProgress.
func PhpCall(method, paramsJSON string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	php.mu.Lock()
	if php.cancel != nil {
		php.cancel() // a new step replaces one still running
	}
	php.cancel = cancel
	php.progress = nil
	php.mu.Unlock()
	defer func() {
		cancel()
		php.mu.Lock()
		php.cancel = nil
		php.mu.Unlock()
	}()
	res := phphost.Call(ctx, method, json.RawMessage(paramsJSON), func(p phphost.Progress) {
		b, _ := json.Marshal(p)
		php.mu.Lock()
		php.progress = append(php.progress, string(b))
		if len(php.progress) > 500 {
			php.progress = php.progress[len(php.progress)-500:]
		}
		php.mu.Unlock()
	})
	return res.JSON()
}

// PhpProgress returns the progress events (phphost.Progress, one JSON object
// per line) since the last call, for a progress bar; "" when there are none.
func PhpProgress() string {
	php.mu.Lock()
	defer php.mu.Unlock()
	out := strings.Join(php.progress, "\n")
	php.progress = nil
	return out
}

// PhpCancel abandons the step PhpCall is running.
func PhpCancel() {
	php.mu.Lock()
	cancel := php.cancel
	php.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
