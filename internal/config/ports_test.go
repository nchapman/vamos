package config

import (
	"strings"
	"testing"
)

func TestServicePorts(t *testing.T) {
	for _, tc := range []struct {
		ports, extra string
		valid        bool
	}{
		{`["3478:3478", "3478:3478/udp", "49160-49170:49160-49170/udp"]`, "", true},
		{`["9000:8080"]`, "    http: {port: 8080}\n", true},
		{`["9000:8080"]`, "    replicas: 2\n", false},
		{`["10000-14095:10000-14095/udp"]`, "", true},
		{`["10000-14096:10000-14096/udp"]`, "", false},
		{`["10000-12047:10000-12047/udp", "13000-15048:13000-15048/udp"]`, "", false},
		{`["0:80"]`, "", false}, {`["65536:80"]`, "", false},
		{`["80:0"]`, "", false}, {`["9000:65536"]`, "", false},
		{`["127.0.0.1:9000:80"]`, "", false}, {`["[::]:9000:80"]`, "", false},
		{`["9000:80/sctp"]`, "", false}, {`["9000:80/TCP"]`, "", false},
		{`["9000-9002:80-81"]`, "", false}, {`["9002-9000:80-82"]`, "", false},
		{`["9000:80", "9000:81/tcp"]`, "", false},
		{`["9000-9002:80-82", "9002:90"]`, "", false},
		{`["80:8080"]`, "", false}, {`["443:8443"]`, "", false},
		{`["443:8443/udp"]`, "", false}, {`["2019:2019"]`, "", false},
		{`["80:8080/udp", "2019:2019/udp"]`, "", true},
		{`["9000"]`, "", false}, {`["+9000:80"]`, "", false},
	} {
		t.Run(tc.ports+tc.extra, func(t *testing.T) {
			_, err := Decode(strings.NewReader("app: turn\nservices:\n  turn:\n    image: coturn/coturn:latest\n    ports: " + tc.ports + "\n" + tc.extra))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}

func TestPortsAcrossServices(t *testing.T) {
	for _, proto := range []string{"tcp", "udp"} {
		_, err := Decode(strings.NewReader("app: turn\nservices:\n  one:\n    image: busybox\n    ports: [\"9000-9002:80-82\"]\n  two:\n    image: busybox\n    ports: [\"9001:90/" + proto + "\"]\n"))
		if (err == nil) != (proto == "udp") {
			t.Fatalf("%s: %v", proto, err)
		}
	}
}
