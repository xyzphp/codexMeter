package main

import (
	"errors"
	"net/url"
	"strings"
)

func proxyPasswordConfigured(raw string) bool {
	parsed, err := parseProxyURL(raw)
	if err != nil || parsed == nil || parsed.User == nil {
		return false
	}
	_, configured := parsed.User.Password()
	return configured
}

// Passwords are write-only through management forms. Omitted credentials keep
// the selected account's existing authentication, never another account's.
func applyProxyUpdate(current string, update ConfigUpdate) (string, error) {
	if update.ProxyURL == nil && update.ProxyUsername == nil && update.ProxyPassword == nil && !update.ProxyClearAuth {
		if _, err := parseProxyURL(current); err != nil {
			return "", err
		}
		return current, nil
	}
	value := current
	if update.ProxyURL != nil {
		value = strings.TrimSpace(*update.ProxyURL)
	}
	next, err := parseProxyURL(value)
	if err != nil {
		return "", err
	}
	if next == nil {
		if (update.ProxyUsername != nil && *update.ProxyUsername != "") || (update.ProxyPassword != nil && *update.ProxyPassword != "") {
			return "", errors.New("使用代理认证时必须填写代理地址")
		}
		return "", nil
	}
	old, err := parseProxyURL(current)
	if err != nil {
		return "", err
	}
	if next.User == nil && old != nil {
		next.User = old.User
	}
	if update.ProxyClearAuth {
		if (update.ProxyUsername != nil && *update.ProxyUsername != "") || (update.ProxyPassword != nil && *update.ProxyPassword != "") {
			return "", errors.New("清除代理认证时不能同时提供用户名或密码")
		}
		next.User = nil
		return next.String(), nil
	}
	username, password, hasPassword := "", "", false
	if next.User != nil {
		username = next.User.Username()
		password, hasPassword = next.User.Password()
		// A masked URL from a configuration view is never a usable password.
		if password == "****" {
			if old == nil || old.User == nil || old.User.Username() != username {
				return "", errors.New("请重新填写代理密码")
			}
			password, hasPassword = old.User.Password()
		}
	}
	if update.ProxyUsername != nil {
		if *update.ProxyUsername != username && update.ProxyPassword == nil && hasPassword {
			return "", errors.New("更改代理用户名时请同时填写新密码")
		}
		username = *update.ProxyUsername
	}
	if update.ProxyPassword != nil {
		password, hasPassword = *update.ProxyPassword, true
	}
	if username == "" {
		if hasPassword {
			return "", errors.New("代理密码需要配合用户名使用；清除认证请使用 proxy_clear_auth")
		}
		next.User = nil
	} else if hasPassword {
		next.User = url.UserPassword(username, password)
	} else {
		next.User = url.User(username)
	}
	return next.String(), nil
}
