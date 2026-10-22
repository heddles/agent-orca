/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

const testRedisPassword = "s3cret-Pa55"

func TestRedactURL(t *testing.T) {
	got := RedactURL("redis://:" + testRedisPassword + "@redis.svc:6379/0")
	if strings.Contains(got, testRedisPassword) {
		t.Fatalf("RedactURL leaked the password: %q", got)
	}
	if !strings.Contains(got, "redis.svc:6379") {
		t.Fatalf("RedactURL dropped the host: %q", got)
	}
	if got := RedactURL("redis://:" + testRedisPassword + "@host:bad port"); strings.Contains(got, testRedisPassword) {
		t.Fatalf("RedactURL leaked the password from an unparseable URL: %q", got)
	}
}

func TestParseRedisURLHidesPassword(t *testing.T) {
	_, err := ParseRedisURL("redis://:" + testRedisPassword + "@host:bad port")
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if strings.Contains(err.Error(), testRedisPassword) {
		t.Fatalf("parse error leaked the password: %v", err)
	}
}

func TestWithPasswordFile(t *testing.T) {
	const file = "/etc/agentorca-redis/password"
	tests := []struct {
		name         string
		url          string
		wantURL      string
		wantPassword string
		wantFile     string
	}{
		{"password only", "redis://:" + testRedisPassword + "@redis.svc:6379", "redis://redis.svc:6379", testRedisPassword, file},
		{"user and password", "redis://app:" + testRedisPassword + "@redis.svc:6379/2", "redis://app@redis.svc:6379/2", testRedisPassword, file},
		{"no password", "redis://redis.svc:6379", "redis://redis.svc:6379", "", ""},
		{"empty", "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, password, err := Config{Backend: "redis", RedisURL: tt.url}.WithPasswordFile(file)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.RedisURL != tt.wantURL || password != tt.wantPassword || got.RedisPasswordFile != tt.wantFile {
				t.Fatalf("got (%q, %q, %q), want (%q, %q, %q)",
					got.RedisURL, password, got.RedisPasswordFile, tt.wantURL, tt.wantPassword, tt.wantFile)
			}
		})
	}
}

func TestNewRedisStoreReadsPasswordFile(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("starting miniredis: %v", err)
	}
	defer mr.Close()
	mr.RequireAuth(testRedisPassword)

	file := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(file, []byte(testRedisPassword+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}

	// The URL carries no password: it must come from the file.
	store, err := newRedisStore(Config{RedisURL: "redis://" + mr.Addr(), RedisPasswordFile: file})
	if err != nil {
		t.Fatalf("connecting with password file: %v", err)
	}
	_ = store.client.Close()

	if _, err := newRedisStore(Config{RedisURL: "redis://" + mr.Addr()}); err == nil {
		t.Fatal("expected auth failure without the password file")
	}
}

func TestNewRedisStoreErrorHidesPassword(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("starting miniredis: %v", err)
	}
	addr := mr.Addr()
	mr.Close() // nothing listening: the connect error path runs

	_, err = newRedisStore(Config{RedisURL: "redis://:" + testRedisPassword + "@" + addr})
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), testRedisPassword) {
		t.Fatalf("connection error leaked the password: %v", err)
	}
}
