package webos

// The registration manifest every webOS remote sends: the permissions plus
// LG's own signature over them. It is the public "LG Remote App" manifest
// used by aiowebostv, LGWebOSRemote and others; newer firmware rejects the
// signature, and then the unsigned form (all permissions, no signature) is
// sent instead.

const signature = "eyJhbGdvcml0aG0iOiJSU0EtU0hBMjU2Iiwia2V5SWQiOiJ0ZXN0LXNpZ25pbm" +
	"ctY2VydCIsInNpZ25hdHVyZVZlcnNpb24iOjF9.hrVRgjCwXVvE2OOSpDZ58hR" +
	"+59aFNwYDyjQgKk3auukd7pcegmE2CzPCa0bJ0ZsRAcKkCTJrWo5iDzNhMBWRy" +
	"aMOv5zWSrthlf7G128qvIlpMT0YNY+n/FaOHE73uLrS/g7swl3/qH/BGFG2Hu4" +
	"RlL48eb3lLKqTt2xKHdCs6Cd4RMfJPYnzgvI4BNrFUKsjkcu+WD4OO2A27Pq1n" +
	"50cMchmcaXadJhGrOqH5YmHdOCj5NSHzJYrsW0HPlpuAx/ECMeIZYDh6RMqaFM" +
	"2DXzdKX9NmmyqzJ3o/0lkk/N97gfVRLW5hA29yeAwaCViZNCP8iC9aO0q9fQoj" +
	"oa7NQnAtw=="

var permissions = []string{
	"LAUNCH", "LAUNCH_WEBAPP", "APP_TO_APP", "CLOSE", "TEST_OPEN", "TEST_PROTECTED",
	"CONTROL_AUDIO", "CONTROL_DISPLAY", "CONTROL_INPUT_JOYSTICK", "CONTROL_INPUT_MEDIA_RECORDING",
	"CONTROL_INPUT_MEDIA_PLAYBACK", "CONTROL_INPUT_TV", "CONTROL_POWER", "CONTROL_TV_SCREEN",
	"READ_APP_STATUS", "READ_CURRENT_CHANNEL", "READ_INPUT_DEVICE_LIST", "READ_NETWORK_STATE",
	"READ_RUNNING_APPS", "READ_TV_CHANNEL_LIST", "WRITE_NOTIFICATION_TOAST", "READ_POWER_STATE",
	"READ_COUNTRY_INFO", "CONTROL_INPUT_TEXT", "CONTROL_MOUSE_AND_KEYBOARD", "READ_INSTALLED_APPS",
	"READ_SETTINGS",
}

var signedPermissions = []string{
	"TEST_SECURE", "CONTROL_INPUT_TEXT", "CONTROL_MOUSE_AND_KEYBOARD", "READ_INSTALLED_APPS",
	"READ_LGE_SDX", "READ_NOTIFICATIONS", "SEARCH", "WRITE_SETTINGS", "WRITE_NOTIFICATION_ALERT",
	"CONTROL_POWER", "READ_CURRENT_CHANNEL", "READ_RUNNING_APPS", "READ_UPDATE_INFO",
	"UPDATE_FROM_REMOTE_APP", "READ_LGE_TV_INPUT_EVENTS", "READ_TV_CURRENT_TIME",
}

// registration builds the "register" message, signed or not.
func registration(clientKey string, signed bool) map[string]any {
	manifest := map[string]any{
		"appVersion":      "1.1",
		"manifestVersion": 1,
	}
	if signed {
		manifest["permissions"] = permissions
		manifest["signatures"] = []map[string]any{{"signature": signature, "signatureVersion": 1}}
		manifest["signed"] = map[string]any{
			"appId":   "com.lge.test",
			"created": "20140509",
			"localizedAppNames": map[string]string{
				"": "LG Remote App", "ko-KR": "리모컨 앱", "zxx-XX": "ЛГ Rэмotэ AПП",
			},
			"localizedVendorNames": map[string]string{"": "LG Electronics"},
			"permissions":          signedPermissions,
			"serial":               "2f930e2d2cfe083771f68e4fe7bb07",
			"vendorId":             "com.lge",
		}
	} else {
		all := map[string]bool{}
		var list []string
		for _, p := range append(append([]string{}, permissions...), signedPermissions...) {
			if !all[p] {
				all[p] = true
				list = append(list, p)
			}
		}
		manifest["permissions"] = list
	}
	payload := map[string]any{
		"forcePairing": false,
		"pairingType":  "PROMPT",
		"manifest":     manifest,
	}
	if clientKey != "" {
		payload["client-key"] = clientKey
	}
	return map[string]any{"type": "register", "id": "register_0", "payload": payload}
}
