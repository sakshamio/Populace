package world

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

type centre struct {
	lat, lon float64
	weight   float64 // roughly log-scaled metro population
	spread   float64 // degrees; larger means more rural hinterland
}

var centres = []centre{
	{35.7, 139.7, 9, 2.2}, {28.6, 77.2, 10, 4.5}, {31.2, 121.5, 9, 2.6},
	{23.8, 90.4, 8, 3.0}, {-23.5, -46.6, 7, 2.8}, {30.0, 31.2, 7, 2.4},
	{19.4, -99.1, 7, 2.6}, {39.9, 116.4, 8, 3.0}, {19.1, 72.9, 9, 3.4},
	{34.7, 135.5, 5, 1.8}, {24.9, 67.0, 7, 2.6}, {29.6, 106.5, 6, 3.2},
	{41.0, 28.9, 6, 2.4}, {-34.6, -58.4, 5, 2.4}, {22.6, 88.4, 7, 3.2},
	{6.5, 3.4, 8, 3.0}, {-4.4, 15.3, 6, 3.4}, {14.6, 121.0, 7, 2.4},
	{39.1, 117.2, 5, 2.2}, {-22.9, -43.2, 5, 2.2}, {23.1, 113.3, 7, 2.6},
	{31.5, 74.3, 6, 2.8}, {55.8, 37.6, 5, 3.6}, {22.5, 114.1, 5, 1.8},
	{12.97, 77.6, 6, 2.6}, {48.9, 2.35, 5, 2.6}, {-6.2, 106.8, 8, 3.0},
	{13.1, 80.3, 5, 2.6}, {-12.0, -77.0, 5, 2.2}, {13.75, 100.5, 5, 3.0},
	{37.6, 127.0, 6, 2.0}, {17.4, 78.5, 5, 2.8}, {51.5, -0.13, 5, 2.8},
	{35.7, 51.4, 5, 2.6}, {41.9, -87.6, 4, 2.6}, {30.6, 104.1, 6, 3.0},
	{32.1, 118.8, 4, 2.4}, {30.6, 114.3, 5, 2.6}, {10.8, 106.7, 5, 2.6},
	{-8.8, 13.2, 4, 2.8}, {23.0, 72.6, 5, 2.6}, {3.14, 101.7, 4, 2.4},
	{34.3, 108.9, 5, 2.8}, {22.3, 114.2, 4, 1.4}, {30.3, 120.2, 4, 2.0},
	{41.8, 123.4, 4, 2.6}, {24.7, 46.7, 4, 3.4}, {33.3, 44.4, 4, 2.8},
	{-33.4, -70.7, 4, 2.2}, {21.2, 72.8, 4, 2.2}, {40.4, -3.7, 4, 3.0},
	{31.3, 120.6, 4, 2.0}, {18.5, 73.9, 4, 2.4}, {45.8, 126.5, 4, 3.0},
	{29.8, -95.4, 3, 2.6}, {32.8, -96.8, 3, 2.6}, {43.7, -79.4, 3, 2.4},
	{1.35, 103.8, 3, 1.0}, {40.0, -75.2, 3, 2.0}, {33.7, -84.4, 3, 2.4},
	{33.6, 130.4, 3, 2.0}, {15.5, 32.5, 4, 3.2}, {41.4, 2.2, 3, 2.0},
	{-26.2, 28.0, 4, 2.8}, {59.9, 30.3, 3, 3.0}, {36.1, 120.4, 4, 2.2},
	{38.9, 121.6, 3, 2.0}, {38.9, -77.0, 3, 2.2}, {16.9, 96.2, 4, 3.2},
	{31.2, 29.9, 4, 2.2}, {36.7, 117.0, 4, 2.6}, {20.7, -103.3, 3, 2.4},
	{40.7, -74.0, 5, 2.4}, {34.05, -118.2, 5, 2.6}, {52.5, 13.4, 4, 3.0},
	{41.9, 12.5, 3, 2.6}, {-33.9, 151.2, 3, 2.0}, {-37.8, 145.0, 3, 2.0},
	{-1.3, 36.8, 4, 3.0}, {9.0, 38.7, 4, 3.2}, {5.6, -0.2, 3, 2.4},
	{33.6, -7.6, 3, 2.4}, {36.8, 3.06, 3, 2.6}, {-6.8, 39.3, 3, 2.6},
	{5.3, -4.0, 3, 2.4}, {34.5, 69.2, 3, 3.0}, {41.3, 69.2, 3, 3.0},
	{50.5, 30.5, 3, 3.2}, {52.2, 21.0, 3, 3.0}, {4.7, -74.1, 4, 2.6},
	{10.5, -66.9, 3, 2.2}, {49.3, -123.1, 2, 2.0}, {-36.9, 174.8, 2, 1.8},
	{52.4, 4.9, 3, 2.0}, {59.3, 18.1, 2, 2.6}, {47.4, 8.5, 2, 1.8},
	{25.3, 55.3, 3, 2.0}, {21.0, 105.8, 4, 2.8}, {26.9, 80.9, 5, 3.4},
	{25.6, 85.1, 5, 3.2}, {27.7, 85.3, 3, 2.0}, {6.9, 79.9, 3, 2.0},
	{-7.2, 112.7, 4, 2.4}, {14.7, -17.5, 3, 2.4}, {12.0, 8.5, 4, 3.0},
	{9.1, 7.5, 3, 2.8}, {-15.8, -47.9, 3, 3.0}, {-19.9, -44.0, 3, 2.6},
	{-30.0, -51.2, 3, 2.4}, {19.0, -70.7, 2, 1.8}, {23.1, -82.4, 2, 1.8},
	{45.5, -73.6, 2, 2.2}, {37.8, -122.4, 3, 2.0}, {47.6, -122.3, 2, 2.2},
	{39.7, -105.0, 2, 2.4}, {33.4, -112.1, 2, 2.4}, {42.4, -71.1, 2, 2.0},
	{35.2, -80.8, 2, 2.4}, {28.6, -81.4, 2, 2.2}, {29.4, -98.5, 2, 2.4},
	{36.2, -115.1, 2, 2.0}, {45.5, -122.7, 2, 2.2}, {43.0, -87.9, 2, 2.2},
	{39.1, -84.5, 2, 2.2}, {35.5, 139.6, 4, 1.8}, {43.1, 141.3, 2, 2.4},
	{54.7, 20.5, 2, 2.4}, {53.4, -2.2, 3, 2.2}, {55.9, -4.3, 2, 2.2},
	{53.5, 9.99, 2, 2.2}, {48.1, 11.6, 2, 2.2}, {50.9, 6.96, 2, 2.2},
	{45.5, 9.19, 3, 2.2}, {40.85, 14.27, 2, 2.0}, {38.0, 23.7, 2, 2.2},
	{46.0, 14.5, 2, 2.0}, {44.4, 26.1, 3, 2.6}, {42.7, 23.3, 2, 2.2},
	{56.9, 24.1, 2, 2.2}, {60.2, 24.9, 2, 2.6}, {59.9, 10.75, 2, 2.4},
	{55.7, 12.6, 2, 2.2}, {64.1, -21.9, 1, 1.6}, {-1.5, -48.5, 2, 2.6},
	{-3.7, -38.5, 3, 2.4}, {-8.05, -34.9, 3, 2.4}, {-12.97, -38.5, 3, 2.4},
	{-25.4, -49.3, 2, 2.2}, {4.6, -74.1, 2, 2.2}, {-16.5, -68.1, 2, 2.4},
	{-0.2, -78.5, 2, 2.2}, {10.0, -84.1, 2, 1.8}, {14.6, -90.5, 3, 2.2},
	{13.7, -89.2, 2, 1.8}, {12.1, -86.2, 2, 2.0}, {18.5, -72.3, 3, 1.8},
	{6.8, -58.2, 1, 2.0}, {-17.8, 31.0, 2, 2.8}, {-25.7, 28.2, 2, 2.4},
	{-33.9, 18.4, 3, 2.2}, {-29.9, 31.0, 2, 2.2}, {0.3, 32.6, 3, 2.8},
	{-3.4, 29.4, 2, 2.2}, {-1.9, 30.1, 2, 2.0}, {11.6, 43.1, 1, 1.8},
	{2.0, 45.3, 2, 2.6}, {15.3, 44.2, 3, 2.6}, {31.9, 35.9, 2, 2.0},
	{33.5, 36.3, 3, 2.4}, {33.9, 35.5, 2, 1.8}, {32.1, 34.8, 3, 2.0},
	{38.4, 27.1, 3, 2.4}, {39.9, 32.9, 3, 2.8}, {40.2, 44.5, 2, 2.2},
	{41.7, 44.8, 2, 2.2}, {40.4, 49.9, 2, 2.2}, {43.2, 76.9, 2, 3.0},
	{51.2, 71.4, 2, 3.4}, {55.0, 82.9, 2, 3.4}, {56.8, 60.6, 2, 3.0},
	{53.2, 50.2, 2, 2.8}, {47.2, 39.7, 2, 2.8}, {45.0, 39.0, 2, 2.8},
	{54.7, 55.9, 2, 2.8}, {54.3, 48.4, 2, 2.6}, {58.0, 56.2, 2, 2.8},
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

// samplePosition returns degrees (lat, lon).
func samplePosition(r *rand, st Stratum) (lon, lat float64) {
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
	lat = c.lat + r.norm()*sig
	lon = c.lon + r.norm()*sig*1.35 // degrees of longitude are shorter

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
	return lon, lat
}
