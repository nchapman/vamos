package ssh

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/nchapman/soar/internal/deploy"
	"github.com/nchapman/soar/internal/process"
)

func TestRollbackTransportWithoutSource(t *testing.T) {
	for _, apply := range []bool{false, true} {
		r := appRequest()
		r.Action, r.Config, r.Apply, r.AllowDowntime, r.Service = "rollback", nil, apply, true, "web"
		preview := &deploy.RollbackPreview{TargetOperation: strings.Repeat("b", 32), NeedsConfirmation: !apply, Services: []deploy.RollbackService{{Name: "web", CurrentImage: "new", TargetImage: "old", Downtime: true, SecretVersions: map[string]string{"TOKEN": "version"}}}}
		run := func(_ context.Context, name string, args []string, input []byte) (process.Result, error) {
			if name != "ssh" || !strings.Contains(args[len(args)-1], "systemd-run") {
				t.Fatal("wrong transport boundary")
			}
			files := map[string][]byte{}
			tr := tar.NewReader(bytes.NewReader(input))
			for {
				h, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				files[h.Name], err = io.ReadAll(tr)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, ok := files["context.tar"]; ok {
				t.Fatal("unexpected source archive")
			}
			var got deploy.Request
			if err := json.Unmarshal(files["request.json"], &got); err != nil || got.Action != r.Action || got.Apply != apply || !got.AllowDowntime || got.Service != "web" || got.Operation != r.Operation {
				t.Fatalf("request=%+v err=%v", got, err)
			}
			data, err := json.Marshal(deploy.Response{Version: 1, RollbackPreview: preview})
			return process.Result{Stdout: data}, err
		}
		response, err := runApp(context.Background(), "root@home", r, []byte("binary"), nil, run)
		if err != nil || response.RollbackPreview == nil || response.RollbackPreview.Services[0].SecretVersions["TOKEN"] != "version" {
			t.Fatalf("response=%+v err=%v", response, err)
		}
	}
}
