package ssh

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/nchapman/vamos/internal/deploy"
	"github.com/nchapman/vamos/internal/process"
	"strings"
	"testing"
)

func TestUnusableResponsesCannotAcknowledgeServiceBindings(t *testing.T) {
	request := appRequest()
	good := deploy.Response{Version: 1, Status: &deploy.Status{Operation: request.Operation, HostIdentity: strings.Repeat("b", 32), Services: map[string]deploy.ServiceStatus{"web": {Incarnation: strings.Repeat("c", 32)}}}}
	data, _ := json.Marshal(good)
	wrong := good
	wrong.Version = 2
	badVersion, _ := json.Marshal(wrong)
	for _, tc := range []struct {
		name string
		data []byte
	}{{"invalid version", badVersion}, {"trailing JSON", append(append([]byte{}, data...), []byte(" {}")...)}, {"lost response", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			response, err := runApp(context.Background(), "root@home", request, []byte("binary"), []byte("context"), func(context.Context, string, []string, []byte) (process.Result, error) {
				return process.Result{Stdout: tc.data}, errors.New("transport failure")
			})
			if err == nil || response.Status != nil {
				t.Fatalf("unusable response exposed an identity acknowledgment: %+v %v", response, err)
			}
		})
	}
	good.Error = "candidate readiness failed"
	data, _ = json.Marshal(good)
	response, err := runApp(context.Background(), "root@home", request, []byte("binary"), []byte("context"), func(context.Context, string, []string, []byte) (process.Result, error) {
		return process.Result{Stdout: data}, errors.New("remote exit 1")
	})
	if err == nil || response.Status == nil || response.Status.Operation != request.Operation {
		t.Fatalf("valid admitted failure lost acknowledgment: %+v %v", response, err)
	}
}
