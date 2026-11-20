package i18n

// API демона: тексты ошибок (коды стабильны, язык — из Accept-Language).
func init() {
	Register(map[string][2]string{
		"api.err.size":         {"size: expected a string (\"4G\") or a number of bytes", "size: ожидается строка (\"4G\") или число байт"},
		"api.err.method":       {"method %s is not supported for %s", "метод %s не поддерживается для %s"},
		"api.err.not_found":    {"no such endpoint: %s", "нет такого эндпоинта: %s"},
		"api.err.forbidden":    {"permission denied: read — root or the ramplus group, changes — root or the ramplus-admin group", "нет прав: чтение — root или группа ramplus, изменения — root или группа ramplus-admin"},
		"api.err.trailing":     {"extra data after JSON", "лишние данные после JSON"},
		"api.err.too_large":    {"request body larger than %d bytes", "тело запроса больше %d байт"},
		"api.err.bad_json":     {"invalid JSON: %v", "неверный JSON: %v"},
		"api.err.action":       {"action: expected apply, resize or remove, got %q", "action: ожидается apply, resize или remove, получено %q"},
		"api.err.existing":     {"%s: expected keep or replace, got %q", "%s: ожидается keep или replace, получено %q"},
		"api.err.plan_id":      {"plan_id: required field (POST /v1/plan/...)", "plan_id: обязательное поле (POST /v1/plan/...)"},
		"api.err.op_not_found": {"operation %q not found (the daemon only knows operations it started; history — GET /v1/history)", "операция %q не найдена (демон помнит только запущенные им операции; история — GET /v1/history)"},
		"api.err.set":          {"set: %v", "set: %v"},
	})
}
