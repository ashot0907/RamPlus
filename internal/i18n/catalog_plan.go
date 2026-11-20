package i18n

// Модель плана: общие шаги, операции, домены, даты.
func init() {
	Register(map[string][2]string{
		"step.write_file":    {"write %s", "записать %s"},
		"step.delete_config": {"delete %s", "удалить %s"},
		"step.mkdir":         {"create directory %s", "создать каталог %s"},
		"step.rmdir":         {"remove directory %s", "удалить каталог %s"},

		"domain.swap":    {"a swap change", "изменение swap"},
		"domain.profile": {"a profile change", "настройка профиля"},
		"domain.setup":   {"a memory setup (swap and profile)", "настройка памяти (swap и профиль)"},

		"action.apply":         {"swap creation", "создание swap"},
		"action.resize":        {"swap resize", "изменение размера swap"},
		"action.remove":        {"swap removal", "удаление swap"},
		"action.profile-apply": {"profile apply", "применение профиля"},
		"action.profile-reset": {"profile reset", "сброс профиля"},
		"action.setup":         {"memory setup", "настройка памяти"},

		"date.day_month_time": {"%[2]s %[1]d %[3]s", "%d %s %s"},
		"date.month.01":       {"January", "января"},
		"date.month.02":       {"February", "февраля"},
		"date.month.03":       {"March", "марта"},
		"date.month.04":       {"April", "апреля"},
		"date.month.05":       {"May", "мая"},
		"date.month.06":       {"June", "июня"},
		"date.month.07":       {"July", "июля"},
		"date.month.08":       {"August", "августа"},
		"date.month.09":       {"September", "сентября"},
		"date.month.10":       {"October", "октября"},
		"date.month.11":       {"November", "ноября"},
		"date.month.12":       {"December", "декабря"},

		"age.now":     {"just now", "только что"},
		"age.minutes": {"%d minute ago|%d minutes ago", "%d минуту назад|%d минуты назад|%d минут назад"},
		"age.hours":   {"%d hour ago|%d hours ago", "%d час назад|%d часа назад|%d часов назад"},
		"age.days":    {"%d day ago|%d days ago", "%d день назад|%d дня назад|%d дней назад"},
	})
}
