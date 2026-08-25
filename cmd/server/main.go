package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"restoration-quality/internal/application"
	"restoration-quality/internal/audit"
	"restoration-quality/internal/httpapi"
	"restoration-quality/internal/persistence"
	"strings"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:19081", "监听地址")
	self := flag.Bool("self-check", false, "执行端到端自检后退出")
	flag.Parse()
	if v := os.Getenv("PORT"); v != "" && flag.Lookup("addr").Value.String() == "127.0.0.1:19081" {
		*addr = "127.0.0.1:" + v
	}
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		dir = filepath.Join("data")
	}
	store, e := persistence.New(dir)
	if e != nil {
		panic(e)
	}
	app := application.New(store, audit.New(dir))
	srv := &http.Server{Addr: *addr, Handler: httpapi.New(app).Routes(), ReadHeaderTimeout: 5 * time.Second}
	if *self {
		go srv.ListenAndServe()
		if e := selfCheck(*addr); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		_ = srv.Shutdown(context.Background())
		return
	}
	go func() {
		if e := srv.ListenAndServe(); e != nil && e != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, e)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
func selfCheck(addr string) error {
	client := &http.Client{Timeout: 3 * time.Second}
	call := func(method, path string, body interface{}) (map[string]interface{}, error) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, "http://"+addr+path, strings.NewReader(string(b)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Actor", "self-check")
		if method == http.MethodPut {
			req.Header.Set("If-Match", "1")
		}
		if strings.Contains(path, "/complete") {
			req.Header.Set("If-Match", "3")
		}
		res, e := client.Do(req)
		if e != nil {
			return nil, e
		}
		defer res.Body.Close()
		var out map[string]interface{}
		_ = json.NewDecoder(res.Body).Decode(&out)
		if res.StatusCode >= 300 {
			return nil, fmt.Errorf("自检接口 %s 返回 %d", path, res.StatusCode)
		}
		return out, nil
	}
	ready := false
	for i := 0; i < 20; i++ {
		res, e := client.Get("http://" + addr + "/healthz")
		if e == nil && res.StatusCode == 200 {
			res.Body.Close()
			ready = true
			break
		}
		if res != nil {
			res.Body.Close()
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		return fmt.Errorf("服务未就绪")
	}
	checkKey := fmt.Sprintf("self-%d", time.Now().UnixNano())
	p, e := call(http.MethodPost, "/v1/projects", map[string]interface{}{"asset_code": "SELF-" + checkKey, "title": "自检文物", "custodian": "系统", "request_id": checkKey})
	if e != nil {
		return e
	}
	id, _ := p["id"].(string)
	_, e = call(http.MethodPut, "/v1/projects/"+id+"/baseline", map[string]interface{}{"plan": "标准修复方案", "materials": []string{"材料A"}, "risk_level": "low"})
	if e != nil {
		return e
	}
	pr, e := call(http.MethodPost, "/v1/projects/"+id+"/procedures", map[string]interface{}{"name": "清理", "technician": "技师", "sequence": 1})
	if e != nil {
		return e
	}
	pid, _ := pr["id"].(string)
	// baseline revision 2 + procedure creation revision 3
	// complete requires the optimistic-lock revision header.
	// The helper applies this header only to mutating requests below.
	now := time.Now().UTC()
	_, e = call(http.MethodPost, "/v1/projects/"+id+"/procedures/"+pid+"/complete", map[string]interface{}{"request_id": "self-complete", "started_at": now.Add(-time.Minute).Format(time.RFC3339), "ended_at": now.Format(time.RFC3339), "environment": "20C", "instruction": "按方案", "result": "完成", "evidence": []map[string]interface{}{{"id": "self-ev", "kind": "photo", "uri": "self://photo", "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "captured_at": now.Format(time.RFC3339), "metadata": map[string]string{"description": "自检照片"}}}})
	return e
}
