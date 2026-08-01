package netinfo

// iataPlace is a city plus its approximate centre coordinate. The coordinate is
// only ever used to centre a nearby-server search (Ookla sorts by distance and
// pings the closest handful), so city-level precision is plenty.
type iataPlace struct {
	City     string
	Lat, Lon float64
}

// iataCity maps airport/city codes common in ISP router hostnames AND Cloudflare
// PoP ("colo") codes to a city + coordinate. Deliberately a curated set of major
// interconnection cities (where traffic hands off), not a full airport database -
// it backs the offline city fallback when RIPE IPmap has no answer for a hop.
// Both true IATA codes (fra, lhr) and common ISP abbreviations (tor, mtl).
// Lower-case keys; lookups are lower-cased.
//
// It used to back ColoCoord as well, which centred the settings
// server-browsing list on the Cloudflare PoP. That rung is gone: the PoP is the
// one place live auto-select can never choose - it races the exit router, the
// ISP geolocation and speedtest.net's own placement of our address (see
// main.autoOrigins) - so centring the picker there offered a pool auto could
// not pick from.
var iataCity = map[string]iataPlace{
	// North America
	"yyz": {"Toronto", 43.65, -79.38}, "tor": {"Toronto", 43.65, -79.38},
	"yul": {"Montreal", 45.50, -73.57}, "mtl": {"Montreal", 45.50, -73.57},
	"yvr": {"Vancouver", 49.28, -123.12}, "van": {"Vancouver", 49.28, -123.12},
	"yyc": {"Calgary", 51.05, -114.07},
	"yow": {"Ottawa", 45.42, -75.70}, "ott": {"Ottawa", 45.42, -75.70},
	"nyc": {"New York", 40.71, -74.01}, "jfk": {"New York", 40.71, -74.01}, "lga": {"New York", 40.71, -74.01}, "ewr": {"Newark", 40.74, -74.17}, "nyk": {"New York", 40.71, -74.01},
	"lax": {"Los Angeles", 34.05, -118.24},
	"sfo": {"San Francisco", 37.77, -122.42}, "sjc": {"San Jose", 37.34, -121.89}, "smf": {"Sacramento", 38.58, -121.49},
	"chi": {"Chicago", 41.88, -87.63}, "ord": {"Chicago", 41.88, -87.63}, "mdw": {"Chicago", 41.88, -87.63},
	"dfw": {"Dallas", 32.78, -96.80}, "dal": {"Dallas", 32.78, -96.80},
	"iad": {"Ashburn", 39.04, -77.49}, "dca": {"Washington", 38.90, -77.04}, "was": {"Washington", 38.90, -77.04},
	"atl": {"Atlanta", 33.75, -84.39},
	"mia": {"Miami", 25.76, -80.19},
	"sea": {"Seattle", 47.61, -122.33},
	"den": {"Denver", 39.74, -104.99},
	"phx": {"Phoenix", 33.45, -112.07},
	"bos": {"Boston", 42.36, -71.06},
	"hou": {"Houston", 29.76, -95.37}, "iah": {"Houston", 29.76, -95.37},
	"slc": {"Salt Lake City", 40.76, -111.89},
	"pdx": {"Portland", 45.52, -122.68},
	"msp": {"Minneapolis", 44.98, -93.27},
	"phl": {"Philadelphia", 39.95, -75.17},
	"det": {"Detroit", 42.33, -83.05},
	// Europe
	"lon": {"London", 51.51, -0.13}, "lhr": {"London", 51.51, -0.13}, "ldn": {"London", 51.51, -0.13},
	"fra": {"Frankfurt", 50.11, 8.68}, "ffm": {"Frankfurt", 50.11, 8.68},
	"ams": {"Amsterdam", 52.37, 4.90},
	"par": {"Paris", 48.86, 2.35}, "cdg": {"Paris", 48.86, 2.35},
	"mad": {"Madrid", 40.42, -3.70},
	"bcn": {"Barcelona", 41.39, 2.17},
	"mil": {"Milan", 45.46, 9.19}, "mxp": {"Milan", 45.46, 9.19},
	"rom": {"Rome", 41.90, 12.50}, "fco": {"Rome", 41.90, 12.50},
	"zrh": {"Zurich", 47.37, 8.54}, "zur": {"Zurich", 47.37, 8.54},
	"gva": {"Geneva", 46.20, 6.14},
	"vie": {"Vienna", 48.21, 16.37},
	"muc": {"Munich", 48.14, 11.58},
	"ber": {"Berlin", 52.52, 13.40},
	"ham": {"Hamburg", 53.55, 9.99},
	"dus": {"Dusseldorf", 51.23, 6.78},
	"sto": {"Stockholm", 59.33, 18.06}, "arn": {"Stockholm", 59.33, 18.06},
	"cph": {"Copenhagen", 55.68, 12.57},
	"osl": {"Oslo", 59.91, 10.75},
	"hel": {"Helsinki", 60.17, 24.94},
	"dub": {"Dublin", 53.35, -6.26},
	"bru": {"Brussels", 50.85, 4.35},
	"lis": {"Lisbon", 38.72, -9.14},
	"waw": {"Warsaw", 52.23, 21.01},
	"prg": {"Prague", 50.08, 14.44},
	"buh": {"Bucharest", 44.43, 26.10}, "otp": {"Bucharest", 44.43, 26.10},
	"sof": {"Sofia", 42.70, 23.32},
	"ath": {"Athens", 37.98, 23.73},
	"ist": {"Istanbul", 41.01, 28.98},
	"mrs": {"Marseille", 43.30, 5.37},
	// Asia-Pacific
	"sin": {"Singapore", 1.35, 103.82}, "sgp": {"Singapore", 1.35, 103.82},
	"hkg": {"Hong Kong", 22.32, 114.17},
	"nrt": {"Tokyo", 35.68, 139.69}, "hnd": {"Tokyo", 35.68, 139.69}, "tyo": {"Tokyo", 35.68, 139.69},
	"icn": {"Seoul", 37.57, 126.98}, "sel": {"Seoul", 37.57, 126.98},
	"syd": {"Sydney", -33.87, 151.21},
	"mel": {"Melbourne", -37.81, 144.96},
	"per": {"Perth", -31.95, 115.86},
	"bne": {"Brisbane", -27.47, 153.03},
	"akl": {"Auckland", -36.85, 174.76},
	"bom": {"Mumbai", 19.08, 72.88}, "mum": {"Mumbai", 19.08, 72.88},
	"del": {"Delhi", 28.61, 77.21},
	"maa": {"Chennai", 13.08, 80.27},
	"blr": {"Bangalore", 12.97, 77.59},
	"tpe": {"Taipei", 25.03, 121.57},
	"kul": {"Kuala Lumpur", 3.14, 101.69},
	"bkk": {"Bangkok", 13.76, 100.50},
	"cgk": {"Jakarta", -6.21, 106.85}, "jkt": {"Jakarta", -6.21, 106.85},
	"mnl": {"Manila", 14.60, 120.98},
	// Middle East / Africa / South America
	"dxb": {"Dubai", 25.20, 55.27}, "auh": {"Abu Dhabi", 24.45, 54.38},
	"jnb": {"Johannesburg", -26.20, 28.05}, "cpt": {"Cape Town", -33.92, 18.42},
	"gru": {"Sao Paulo", -23.55, -46.63}, "sao": {"Sao Paulo", -23.55, -46.63},
	"gig": {"Rio de Janeiro", -22.91, -43.17}, "rio": {"Rio de Janeiro", -22.91, -43.17},
	"eze": {"Buenos Aires", -34.60, -58.38}, "bue": {"Buenos Aires", -34.60, -58.38},
	"scl": {"Santiago", -33.45, -70.67},
	"bog": {"Bogota", 4.71, -74.07},
	"lim": {"Lima", -12.05, -77.04},
	"mex": {"Mexico City", 19.43, -99.13},
}
