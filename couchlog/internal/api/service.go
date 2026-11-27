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
	{[]string{"amazon", "primevideo", "prime video"}, ServiceInfo{Key: "prime", Name: "Prime Video"}},
	{[]string{"appletv", "apple tv"}, ServiceInfo{Key: "appletv", Name: "Apple TV+"}},
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
