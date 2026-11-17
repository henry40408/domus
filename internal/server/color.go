package server

import "math"

// rgbToXY converts sRGB (0-255 each) to CIE xy, using the wide-gamut matrix
// Hue documents.
func rgbToXY(r, g, b float64) (x, y float64) {
	lr, lg, lb := linearChannel(r), linearChannel(g), linearChannel(b)
	cx := lr*0.664511 + lg*0.154324 + lb*0.162028
	cy := lr*0.283881 + lg*0.668433 + lb*0.047685
	cz := lr*0.000088 + lg*0.072310 + lb*0.986039
	sum := cx + cy + cz
	if sum == 0 {
		return 0.3127, 0.3290 // black has no chromaticity: fall back to the white point
	}
	return round4(cx / sum), round4(cy / sum)
}

func linearChannel(c float64) float64 {
	c = math.Min(math.Max(c/255, 0), 1)
	if c > 0.04045 {
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return c / 12.92
}

func round4(v float64) float64 { return math.Round(v*10_000) / 10_000 }

// hsToRGB converts Home Assistant's hs_color (hue 0-360, saturation 0-100) at
// full value to sRGB.
func hsToRGB(h, s float64) (r, g, b float64) {
	c := math.Min(math.Max(s/100, 0), 1)
	sector := remEuclid(h, 360) / 60
	x := c * (1 - math.Abs(math.Mod(sector, 2)-1))
	m := 1 - c
	switch int(sector) {
	case 0:
		r, g, b = c, x, 0
	case 1:
		r, g, b = x, c, 0
	case 2:
		r, g, b = 0, c, x
	case 3:
		r, g, b = 0, x, c
	case 4:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	return (r + m) * 255, (g + m) * 255, (b + m) * 255
}

// remEuclid is Rust's f64::rem_euclid: the result is never negative.
func remEuclid(v, divisor float64) float64 {
	r := math.Mod(v, divisor)
	if r < 0 {
		return r + math.Abs(divisor)
	}
	return r
}
