package api

import "strings"

// Service groups apps the way people think of them: Netflix on the LG and
// Netflix on the box are one service.
type ServiceInfo struct {
	Key  string `json:"key"`  // netflix, hbomax, plex, youtube, tubi, or the app id
	Name string `json:"name"` // Netflix, HBO Max, ...
	// Free is how the service is paid for when it is not a subscription:
	// "library" (Plex, your own files), "free", "ads"; "" = you pay for it.
	Free string `json:"free,omitempty"`
}

var known = []struct {
	match []string
	info  ServiceInfo
}{
	{[]string{"netflix"}, ServiceInfo{Key: "netflix", Name: "Netflix"}},
	{[]string{"hbo", "wbd"}, ServiceInfo{Key: "hbomax", Name: "HBO Max"}},
	{[]string{"plex"}, ServiceInfo{Key: "plex", Name: "Plex", Free: "library"}},
	{[]string{"youtube"}, ServiceInfo{Key: "youtube", Name: "YouTube", Free: "free"}},
	{[]string{"tubi"}, ServiceInfo{Key: "tubi", Name: "Tubi", Free: "ads"}},
	{[]string{"disney"}, ServiceInfo{Key: "disney", Name: "Disney+"}},
	{[]string{"amazon", "primevideo", "prime video", "prime"}, ServiceInfo{Key: "prime", Name: "Prime Video"}},
	{[]string{"hulu"}, ServiceInfo{Key: "hulu", Name: "Hulu"}},
	{[]string{"peacock"}, ServiceInfo{Key: "peacock", Name: "Peacock"}},
	{[]string{"starz"}, ServiceInfo{Key: "starz", Name: "STARZ"}},
	{[]string{"appletv", "apple tv", "apple"}, ServiceInfo{Key: "appletv", Name: "Apple TV+"}},
	{[]string{"paramount"}, ServiceInfo{Key: "paramount", Name: "Paramount+"}},
	{[]string{"crunchyroll"}, ServiceInfo{Key: "crunchyroll", Name: "Crunchyroll"}},
	{[]string{"pluto"}, ServiceInfo{Key: "pluto", Name: "Pluto TV", Free: "ads"}},
	{[]string{"vix"}, ServiceInfo{Key: "vix", Name: "ViX"}},
	{[]string{"mubi"}, ServiceInfo{Key: "mubi", Name: "MUBI"}},
}

// ServiceOf names the service behind an app id / name on any device.
func ServiceOf(appID, appName string) ServiceInfo {
	s := strings.ToLower(appID + " " + appName)
	for _, k := range known {
		for _, m := range k.match {
			if strings.Contains(s, m) {
				return k.info
			}
		}
	}
	name := appName
	if name == "" {
		name = appID
	}
	return ServiceInfo{Key: appID, Name: name}
}

// keyFor turns a name you type ("Disney Plus", "disney+") into a service:
// a known one when it matches, else a short key made from the name.
func keyFor(name string) ServiceInfo {
	if info := ServiceOf("", name); info.Key != "" {
		for _, k := range known {
			if k.info.Key == info.Key {
				return info
			}
		}
	}
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return ServiceInfo{Key: b.String(), Name: strings.TrimSpace(name)}
}

func knownInfo(key string) (ServiceInfo, bool) {
	for _, k := range known {
		if k.info.Key == key {
			return k.info, true
		}
	}
	return ServiceInfo{}, false
}
