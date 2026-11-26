package resources

import _ "embed"

//go:embed quota_page.html
var QuotaPage string

//go:embed settings_form.html
var SettingsForm string

//go:embed settings_styles.css
var SettingsStyles string

//go:embed settings_script.js
var SettingsScript string
