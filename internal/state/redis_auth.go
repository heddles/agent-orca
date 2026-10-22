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
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/redis/go-redis/v9"
)

// RedactURL returns rawURL with any password replaced by "xxxxx", safe to put in
// errors and logs. A URL that does not parse is replaced wholesale, since it may
// still contain a password.
func RedactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<unparseable redis URL>"
	}
	return u.Redacted()
}

// WithPasswordFile moves the Redis password out of the config. It returns a copy
// of c whose RedisURL has the password removed and whose RedisPasswordFile is set
// to passwordFile, together with the password itself. When RedisURL carries no
// password, c is returned unchanged with an empty password.
//
// The operator uses this so the password reaches the model-router through a
// mounted Secret instead of the plain-text router ConfigMap.
func (c Config) WithPasswordFile(passwordFile string) (Config, string, error) {
	if c.RedisURL == "" {
		return c, "", nil
	}
	u, err := url.Parse(c.RedisURL)
	if err != nil {
		return c, "", errors.New("parsing redis URL: malformed URL")
	}
	password, ok := u.User.Password()
	if !ok || password == "" {
		return c, "", nil
	}
	if username := u.User.Username(); username != "" {
		u.User = url.User(username)
	} else {
		u.User = nil
	}
	c.RedisURL = u.String()
	c.RedisPasswordFile = passwordFile
	return c, password, nil
}

// ParseRedisURL wraps redis.ParseURL so the error never echoes the URL back:
// a *url.Error from net/url embeds the full input, password included.
func ParseRedisURL(rawURL string) (*redis.Options, error) {
	opts, err := redis.ParseURL(rawURL)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			ue.URL = "<redacted>"
		}
		return nil, fmt.Errorf("parsing redis URL: %w", err)
	}
	return opts, nil
}

// readPasswordFile reads a Redis password mounted from a Secret, trimming the
// trailing newline that tools commonly leave in secret files.
func readPasswordFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading redis password file: %w", err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}
