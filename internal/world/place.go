package world

import "math"

// Placement.
//
// Uniformly scattered points on a sphere read as noise and instantly look
// fake. Real population is extraordinarily clustered, and reproducing that
// clustering is most of what makes a globe look like Earth: the Ganges plain,
// the Nile, the Java corridor, the Chinese east, the empty Sahara and Siberia.
//
// The production path is an inverse-CDF sample over a GHS-POP or WorldPop
// raster at ~1 km, built offline and shipped as a CDF rather than a raster.
// This file is the stand-in: ~180 weighted centres with Gaussian spread, which
// gets the continents right and is a drop-in replacement for the raster
// sampler behind samplePosition.

// A centre is a named population cluster. The name is not decoration: an
// event that breaks "in Lagos" is legible in a way that one breaking at
// spatial rank 4,712,883 is not, and the region is what makes an archetype
// plausible rather than generic -- a smallholder farmer and a finance
// professional are not drawn from the same distribution in Kinshasa and Zurich.
type centre struct {
	name   string
	region Region
	lat    float64
	lon    float64
	weight float64 // roughly log-scaled metro population
	spread float64 // degrees; larger means more rural hinterland
}

var centres = []centre{
	{"Tokyo", EastAsia, 35.7, 139.7, 9, 2.2},
	{"Delhi", SouthAsia, 28.6, 77.2, 10, 4.5},
	{"Shanghai", EastAsia, 31.2, 121.5, 9, 2.6},
	{"Dhaka", SouthAsia, 23.8, 90.4, 8, 3.0},
	{"Sao Paulo", LatinAmerica, -23.5, -46.6, 7, 2.8},
	{"Cairo", MENA, 30.0, 31.2, 7, 2.4},
	{"Mexico City", LatinAmerica, 19.4, -99.1, 7, 2.6},
	{"Beijing", EastAsia, 39.9, 116.4, 8, 3.0},
	{"Mumbai", SouthAsia, 19.1, 72.9, 9, 3.4},
	{"Osaka", EastAsia, 34.7, 135.5, 5, 1.8},
	{"Karachi", SouthAsia, 24.9, 67.0, 7, 2.6},
	{"Chongqing", EastAsia, 29.6, 106.5, 6, 3.2},
	{"Istanbul", MENA, 41.0, 28.9, 6, 2.4},
	{"Buenos Aires", LatinAmerica, -34.6, -58.4, 5, 2.4},
	{"Kolkata", SouthAsia, 22.6, 88.4, 7, 3.2},
	{"Lagos", SubSaharanAfrica, 6.5, 3.4, 8, 3.0},
	{"Kinshasa", SubSaharanAfrica, -4.4, 15.3, 6, 3.4},
	{"Manila", SoutheastAsia, 14.6, 121.0, 7, 2.4},
	{"Tianjin", EastAsia, 39.1, 117.2, 5, 2.2},
	{"Rio de Janeiro", LatinAmerica, -22.9, -43.2, 5, 2.2},
	{"Guangzhou", EastAsia, 23.1, 113.3, 7, 2.6},
	{"Lahore", SouthAsia, 31.5, 74.3, 6, 2.8},
	{"Moscow", Eurasia, 55.8, 37.6, 5, 3.6},
	{"Shenzhen", EastAsia, 22.5, 114.1, 5, 1.8},
	{"Bengaluru", SouthAsia, 12.97, 77.6, 6, 2.6},
	{"Paris", Europe, 48.9, 2.35, 5, 2.6},
	{"Jakarta", SoutheastAsia, -6.2, 106.8, 8, 3.0},
	{"Chennai", SouthAsia, 13.1, 80.3, 5, 2.6},
	{"Lima", LatinAmerica, -12.0, -77.0, 5, 2.2},
	{"Bangkok", SoutheastAsia, 13.75, 100.5, 5, 3.0},
	{"Seoul", EastAsia, 37.6, 127.0, 6, 2.0},
	{"Hyderabad", SouthAsia, 17.4, 78.5, 5, 2.8},
	{"London", Europe, 51.5, -0.13, 5, 2.8},
	{"Tehran", MENA, 35.7, 51.4, 5, 2.6},
	{"Chicago", NorthAmerica, 41.9, -87.6, 4, 2.6},
	{"Chengdu", EastAsia, 30.6, 104.1, 6, 3.0},
	{"Nanjing", EastAsia, 32.1, 118.8, 4, 2.4},
	{"Wuhan", EastAsia, 30.6, 114.3, 5, 2.6},
	{"Ho Chi Minh City", SoutheastAsia, 10.8, 106.7, 5, 2.6},
	{"Luanda", SubSaharanAfrica, -8.8, 13.2, 4, 2.8},
	{"Ahmedabad", SouthAsia, 23.0, 72.6, 5, 2.6},
	{"Kuala Lumpur", SoutheastAsia, 3.14, 101.7, 4, 2.4},
	{"Xi'an", EastAsia, 34.3, 108.9, 5, 2.8},
	{"Hong Kong", EastAsia, 22.3, 114.2, 4, 1.4},
	{"Hangzhou", EastAsia, 30.3, 120.2, 4, 2.0},
	{"Shenyang", EastAsia, 41.8, 123.4, 4, 2.6},
	{"Riyadh", MENA, 24.7, 46.7, 4, 3.4},
	{"Baghdad", MENA, 33.3, 44.4, 4, 2.8},
	{"Santiago", LatinAmerica, -33.4, -70.7, 4, 2.2},
	{"Surat", SouthAsia, 21.2, 72.8, 4, 2.2},
	{"Madrid", Europe, 40.4, -3.7, 4, 3.0},
	{"Suzhou", EastAsia, 31.3, 120.6, 4, 2.0},
	{"Pune", SouthAsia, 18.5, 73.9, 4, 2.4},
	{"Harbin", EastAsia, 45.8, 126.5, 4, 3.0},
	{"Houston", NorthAmerica, 29.8, -95.4, 3, 2.6},
	{"Dallas", NorthAmerica, 32.8, -96.8, 3, 2.6},
	{"Toronto", NorthAmerica, 43.7, -79.4, 3, 2.4},
	{"Singapore", SoutheastAsia, 1.35, 103.8, 3, 1.0},
	{"Philadelphia", NorthAmerica, 40.0, -75.2, 3, 2.0},
	{"Atlanta", NorthAmerica, 33.7, -84.4, 3, 2.4},
	{"Fukuoka", EastAsia, 33.6, 130.4, 3, 2.0},
	{"Khartoum", MENA, 15.5, 32.5, 4, 3.2},
	{"Barcelona", Europe, 41.4, 2.2, 3, 2.0},
	{"Johannesburg", SubSaharanAfrica, -26.2, 28.0, 4, 2.8},
	{"St Petersburg", Eurasia, 59.9, 30.3, 3, 3.0},
	{"Qingdao", EastAsia, 36.1, 120.4, 4, 2.2},
	{"Dalian", EastAsia, 38.9, 121.6, 3, 2.0},
	{"Washington", NorthAmerica, 38.9, -77.0, 3, 2.2},
	{"Yangon", SoutheastAsia, 16.9, 96.2, 4, 3.2},
	{"Alexandria", MENA, 31.2, 29.9, 4, 2.2},
	{"Jinan", EastAsia, 36.7, 117.0, 4, 2.6},
	{"Guadalajara", LatinAmerica, 20.7, -103.3, 3, 2.4},
	{"New York", NorthAmerica, 40.7, -74.0, 5, 2.4},
	{"Los Angeles", NorthAmerica, 34.05, -118.2, 5, 2.6},
	{"Berlin", Europe, 52.5, 13.4, 4, 3.0},
	{"Rome", Europe, 41.9, 12.5, 3, 2.6},
	{"Sydney", Oceania, -33.9, 151.2, 3, 2.0},
	{"Melbourne", Oceania, -37.8, 145.0, 3, 2.0},
	{"Nairobi", SubSaharanAfrica, -1.3, 36.8, 4, 3.0},
	{"Addis Ababa", SubSaharanAfrica, 9.0, 38.7, 4, 3.2},
	{"Accra", SubSaharanAfrica, 5.6, -0.2, 3, 2.4},
	{"Casablanca", MENA, 33.6, -7.6, 3, 2.4},
	{"Algiers", MENA, 36.8, 3.06, 3, 2.6},
	{"Dar es Salaam", SubSaharanAfrica, -6.8, 39.3, 3, 2.6},
	{"Abidjan", SubSaharanAfrica, 5.3, -4.0, 3, 2.4},
	{"Kabul", SouthAsia, 34.5, 69.2, 3, 3.0},
	{"Tashkent", Eurasia, 41.3, 69.2, 3, 3.0},
	{"Kyiv", Europe, 50.5, 30.5, 3, 3.2},
	{"Warsaw", Europe, 52.2, 21.0, 3, 3.0},
	{"Bogota", LatinAmerica, 4.7, -74.1, 4, 2.6},
	{"Caracas", LatinAmerica, 10.5, -66.9, 3, 2.2},
	{"Vancouver", NorthAmerica, 49.3, -123.1, 2, 2.0},
	{"Auckland", Oceania, -36.9, 174.8, 2, 1.8},
	{"Amsterdam", Europe, 52.4, 4.9, 3, 2.0},
	{"Stockholm", Europe, 59.3, 18.1, 2, 2.6},
	{"Zurich", Europe, 47.4, 8.5, 2, 1.8},
	{"Dubai", MENA, 25.3, 55.3, 3, 2.0},
	{"Hanoi", SoutheastAsia, 21.0, 105.8, 4, 2.8},
	{"Lucknow", SouthAsia, 26.9, 80.9, 5, 3.4},
	{"Patna", SouthAsia, 25.6, 85.1, 5, 3.2},
	{"Kathmandu", SouthAsia, 27.7, 85.3, 3, 2.0},
	{"Colombo", SouthAsia, 6.9, 79.9, 3, 2.0},
	{"Surabaya", SoutheastAsia, -7.2, 112.7, 4, 2.4},
	{"Dakar", SubSaharanAfrica, 14.7, -17.5, 3, 2.4},
	{"Kano", SubSaharanAfrica, 12.0, 8.5, 4, 3.0},
	{"Abuja", SubSaharanAfrica, 9.1, 7.5, 3, 2.8},
	{"Brasilia", LatinAmerica, -15.8, -47.9, 3, 3.0},
	{"Belo Horizonte", LatinAmerica, -19.9, -44.0, 3, 2.6},
	{"Porto Alegre", LatinAmerica, -30.0, -51.2, 3, 2.4},
	{"Santo Domingo", LatinAmerica, 19.0, -70.7, 2, 1.8},
	{"Havana", LatinAmerica, 23.1, -82.4, 2, 1.8},
	{"Montreal", NorthAmerica, 45.5, -73.6, 2, 2.2},
	{"San Francisco", NorthAmerica, 37.8, -122.4, 3, 2.0},
	{"Seattle", NorthAmerica, 47.6, -122.3, 2, 2.2},
	{"Denver", NorthAmerica, 39.7, -105.0, 2, 2.4},
	{"Phoenix", NorthAmerica, 33.4, -112.1, 2, 2.4},
	{"Boston", NorthAmerica, 42.4, -71.1, 2, 2.0},
	{"Charlotte", NorthAmerica, 35.2, -80.8, 2, 2.4},
	{"Orlando", NorthAmerica, 28.6, -81.4, 2, 2.2},
	{"San Antonio", NorthAmerica, 29.4, -98.5, 2, 2.4},
	{"Las Vegas", NorthAmerica, 36.2, -115.1, 2, 2.0},
	{"Portland", NorthAmerica, 45.5, -122.7, 2, 2.2},
	{"Milwaukee", NorthAmerica, 43.0, -87.9, 2, 2.2},
	{"Cincinnati", NorthAmerica, 39.1, -84.5, 2, 2.2},
	{"Yokohama", EastAsia, 35.5, 139.6, 4, 1.8},
	{"Sapporo", EastAsia, 43.1, 141.3, 2, 2.4},
	{"Kaliningrad", Eurasia, 54.7, 20.5, 2, 2.4},
	{"Manchester", Europe, 53.4, -2.2, 3, 2.2},
	{"Glasgow", Europe, 55.9, -4.3, 2, 2.2},
	{"Hamburg", Europe, 53.5, 9.99, 2, 2.2},
	{"Munich", Europe, 48.1, 11.6, 2, 2.2},
	{"Cologne", Europe, 50.9, 6.96, 2, 2.2},
	{"Milan", Europe, 45.5, 9.19, 3, 2.2},
	{"Naples", Europe, 40.85, 14.27, 2, 2.0},
	{"Athens", Europe, 38.0, 23.7, 2, 2.2},
	{"Ljubljana", Europe, 46.0, 14.5, 2, 2.0},
	{"Bucharest", Europe, 44.4, 26.1, 3, 2.6},
	{"Sofia", Europe, 42.7, 23.3, 2, 2.2},
	{"Riga", Europe, 56.9, 24.1, 2, 2.2},
	{"Helsinki", Europe, 60.2, 24.9, 2, 2.6},
	{"Oslo", Europe, 59.9, 10.75, 2, 2.4},
	{"Copenhagen", Europe, 55.7, 12.6, 2, 2.2},
	{"Reykjavik", Europe, 64.1, -21.9, 1, 1.6},
	{"Belem", LatinAmerica, -1.5, -48.5, 2, 2.6},
	{"Fortaleza", LatinAmerica, -3.7, -38.5, 3, 2.4},
	{"Recife", LatinAmerica, -8.05, -34.9, 3, 2.4},
	{"Salvador", LatinAmerica, -12.97, -38.5, 3, 2.4},
	{"Curitiba", LatinAmerica, -25.4, -49.3, 2, 2.2},
	{"Soacha", LatinAmerica, 4.6, -74.1, 2, 2.2},
	{"La Paz", LatinAmerica, -16.5, -68.1, 2, 2.4},
	{"Quito", LatinAmerica, -0.2, -78.5, 2, 2.2},
	{"San Jose", LatinAmerica, 10.0, -84.1, 2, 1.8},
	{"Guatemala City", LatinAmerica, 14.6, -90.5, 3, 2.2},
	{"San Salvador", LatinAmerica, 13.7, -89.2, 2, 1.8},
	{"Managua", LatinAmerica, 12.1, -86.2, 2, 2.0},
	{"Port-au-Prince", LatinAmerica, 18.5, -72.3, 3, 1.8},
	{"Georgetown", LatinAmerica, 6.8, -58.2, 1, 2.0},
	{"Harare", SubSaharanAfrica, -17.8, 31.0, 2, 2.8},
	{"Pretoria", SubSaharanAfrica, -25.7, 28.2, 2, 2.4},
	{"Cape Town", SubSaharanAfrica, -33.9, 18.4, 3, 2.2},
	{"Durban", SubSaharanAfrica, -29.9, 31.0, 2, 2.2},
	{"Kampala", SubSaharanAfrica, 0.3, 32.6, 3, 2.8},
	{"Bujumbura", SubSaharanAfrica, -3.4, 29.4, 2, 2.2},
	{"Kigali", SubSaharanAfrica, -1.9, 30.1, 2, 2.0},
	{"Djibouti", SubSaharanAfrica, 11.6, 43.1, 1, 1.8},
	{"Mogadishu", SubSaharanAfrica, 2.0, 45.3, 2, 2.6},
	{"Sanaa", MENA, 15.3, 44.2, 3, 2.6},
	{"Amman", MENA, 31.9, 35.9, 2, 2.0},
	{"Damascus", MENA, 33.5, 36.3, 3, 2.4},
	{"Beirut", MENA, 33.9, 35.5, 2, 1.8},
	{"Tel Aviv", MENA, 32.1, 34.8, 3, 2.0},
	{"Izmir", MENA, 38.4, 27.1, 3, 2.4},
	{"Ankara", MENA, 39.9, 32.9, 3, 2.8},
	{"Yerevan", Eurasia, 40.2, 44.5, 2, 2.2},
	{"Tbilisi", Eurasia, 41.7, 44.8, 2, 2.2},
	{"Baku", Eurasia, 40.4, 49.9, 2, 2.2},
	{"Almaty", Eurasia, 43.2, 76.9, 2, 3.0},
	{"Astana", Eurasia, 51.2, 71.4, 2, 3.4},
	{"Novosibirsk", Eurasia, 55.0, 82.9, 2, 3.4},
	{"Yekaterinburg", Eurasia, 56.8, 60.6, 2, 3.0},
	{"Samara", Eurasia, 53.2, 50.2, 2, 2.8},
	{"Rostov-on-Don", Eurasia, 47.2, 39.7, 2, 2.8},
	{"Krasnodar", Eurasia, 45.0, 39.0, 2, 2.8},
	{"Ufa", Eurasia, 54.7, 55.9, 2, 2.8},
	{"Ulyanovsk", Eurasia, 54.3, 48.4, 2, 2.6},
	{"Perm", Eurasia, 58.0, 56.2, 2, 2.8},
}

var centreCum []float64

func init() {
	centreCum = make([]float64, len(centres))
	acc := 0.0
	for i, c := range centres {
		acc += c.weight
		centreCum[i] = acc
	}
}

// spreadScale widens or tightens placement by stratum.
//
// Nomadic populations are by definition not clustered in metros, so they get a
// much wider spread around their centre; media and high-net-worth personas
// concentrate hard into the largest cities. This is a crude proxy for what a
// real build gets from joining the raster cell to its admin region and an
// urban/rural classification.
var spreadScale = [NumStrata]float64{
	General:      1.0,
	Nomadic:      4.5,
	Immigrant:    0.8,
	HighNetWorth: 0.45,
	Media:        0.35,
}

// PlaceName is where a persona lives. Used to say "this broke in Lagos" rather
// than quoting a spatial rank, which is the difference between a simulation
// someone can reason about and one they can only watch.
func (w *World) PlaceName(i int) string {
	p := int(w.Place[i])
	if p < 0 || p >= len(centres) {
		return "unknown"
	}
	return centres[p].name
}

// PlaceRegion is the region a persona's centre sits in.
func (w *World) PlaceRegion(i int) Region { return centres[w.Place[i]].region }

// NumPlaces reports the size of the centre table.
func NumPlaces() int { return len(centres) }

// PlaceInfo is one named centre, for the API.
func PlaceInfo(i int) (name string, region Region, lat, lon float64) {
	c := centres[i]
	return c.name, c.region, c.lat, c.lon
}

// samplePosition returns degrees (lat, lon) and the centre it was drawn from,
// plus how far out it landed in units of that centre's spread. Urbanity is
// returned rather than recomputed later because the caller needs it to choose
// an archetype, and a second distance calculation would be a second chance to
// disagree with this one.
func samplePosition(r *rand, st Stratum) (lon, lat float64, place uint16, urbanity float32) {
	total := centreCum[len(centreCum)-1]

	// Media and high-net-worth personas bias toward the heaviest centres
	// rather than sampling the full distribution: broadcast nodes cluster.
	p := r.f64() * total
	if st == Media || st == HighNetWorth {
		q := r.f64()
		p = (1 - q*q) * total // skew toward the high-weight tail of the CDF
	}

	k := 0
	for k < len(centreCum)-1 && centreCum[k] < p {
		k++
	}
	c := centres[k]

	sig := c.spread * spreadScale[st]
	dlat, dlon := r.norm(), r.norm()
	lat = c.lat + dlat*sig
	lon = c.lon + dlon*sig*1.35 // degrees of longitude are shorter

	// Urbanity falls off with distance from the centre in units of its own
	// spread, so a person two spreads out of Dhaka and two out of Reykjavik are
	// both "rural" for that place rather than for the planet.
	d := math.Sqrt(dlat*dlat + dlon*dlon)
	urbanity = float32(math.Exp(-d * 0.9))
	place = uint16(k)

	// Clamp latitude rather than wrapping: wrapping would teleport people
	// across the pole and put population in the Arctic Ocean.
	if lat > 84 {
		lat = 84
	}
	if lat < -84 {
		lat = -84
	}
	for lon > 180 {
		lon -= 360
	}
	for lon < -180 {
		lon += 360
	}
	return lon, lat, place, urbanity
}
