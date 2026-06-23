package secrets

import "net/url"

func MaskURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	username := u.User.Username()
	if _, ok := u.User.Password(); ok {
		u.User = url.UserPassword(username, "****")
	}
	return u.String()
}
