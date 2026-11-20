package i18n

// ramplus-web, пароль веба, блокировка.
func init() {
	Register(map[string][2]string{
		"web.err.daemon_unavailable": {"the ramplusd daemon is unavailable: %v — sudo systemctl start ramplusd", "демон ramplusd недоступен: %v — sudo systemctl start ramplusd"},
		"web.err.not_found":          {"no such path: %s", "нет такого пути: %s"},
		"web.err.host":               {"Host %q is not allowed: use http://127.0.0.1:7781 or add the name with --host", "Host %q не разрешён: используйте http://127.0.0.1:7781 или добавьте имя через --host"},
		"web.err.encoded_path":       {"encoded characters in the path are not supported", "закодированные символы в пути не поддерживаются"},
		"web.err.not_configured":     {"the web interface password is not set: run sudo ramplus init", "пароль веб-интерфейса не задан: выполните sudo ramplus init"},
		"web.err.auth_unreadable":    {"could not read the password file: %v (ramplus-web must be in the %s group)", "не удалось прочитать файл пароля: %v (ramplus-web должен быть в группе %s)"},
		"web.err.login_required":     {"login required", "нужно войти"},
		"web.err.csrf":               {"missing or invalid CSRF token (X-CSRF-Token)", "нет или неверный CSRF-токен (X-CSRF-Token)"},
		"web.err.bad_json":           {"invalid JSON", "неверный JSON"},
		"web.err.bad_password":       {"wrong password", "неверный пароль"},
		"web.err.bad_password_pause": {"wrong password; too many attempts — pause of %d s", "неверный пароль; слишком много попыток — пауза %d с"},
		"web.err.rate_limited":       {"too many failed attempts — try again in %d s", "слишком много неудачных попыток — попробуйте через %d с"},
		"web.err.method":             {"method not supported", "метод не поддерживается"},
		"web.err.content_type":       {"expected Content-Type: application/json", "ожидается Content-Type: application/json"},
		"web.err.cross_site":         {"cross-site request rejected", "запрос с другого сайта отклонён"},

		"webauth.err.format":  {"invalid hash format (expected $argon2id$v=19$m=…,t=…,p=…$salt$hash)", "неверный формат хеша (ожидается $argon2id$v=19$m=…,t=…,p=…$salt$hash)"},
		"webauth.err.bad":     {"wrong password", "пароль не подходит"},
		"webauth.err.utf8":    {"the password must be UTF-8", "пароль должен быть в UTF-8"},
		"webauth.err.newline": {"the password cannot contain a line break", "пароль не может содержать перевод строки"},
		"webauth.err.short":   {"the password is shorter than %d characters", "пароль короче %d символов"},
		"webauth.err.long":    {"the password is longer than %d bytes", "пароль длиннее %d байт"},
		"webauth.err.spaces":  {"the password is only spaces", "пароль из одних пробелов"},
		"webauth.err.no_hash": {"the password file has no hash", "в файле пароля нет хеша"},

		"sys.locked":      {"another ramplus instance is already running an operation", "другой экземпляр ramplus уже выполняет операцию"},
		"sys.locked_path": {"%v (%s)", "%v (%s)"},
	})
}
