package main

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"

	"github.com/disintegration/gift"
)

var white = color.NRGBAModel.Convert(color.White)
var black = color.NRGBAModel.Convert(color.Black)
var ErrNotFound = errors.New("not found")

type Primed struct {
	Data     []byte
	Skeleton Skeleton
}

type Skeleton struct {
	W, H int
	Rows Rows
	Cols Cols
}

type Rows []*Row

type Row struct {
	Num    int
	Top    int
	Bottom int
}

func (rs Rows) Find(y int) (int, error) {
	for _, r := range rs {
		if y >= r.Top && y <= r.Bottom {
			return r.Num, nil
		}
	}
	return 0, ErrNotFound
}

func (rs Rows) Shift(offset int) {
	for _, r := range rs {
		r.Top += offset
		r.Bottom += offset
	}
}

type Cols []*Col

type Col struct {
	Num   int
	Left  int
	Right int
}

func (cs Cols) Find(x int) (int, error) {
	for _, c := range cs {
		if x >= c.Left && x <= c.Right {
			return c.Num, nil
		}
	}
	return 0, ErrNotFound
}

func (cs Cols) Shift(offset int) {
	for _, c := range cs {
		c.Left += offset
		c.Right += offset
	}
}

func (c *Col) Split() Cols {
	size := c.Right - c.Left
	c1 := &Col{
		Num:   c.Num,
		Left:  c.Left,
		Right: c.Right - size/2,
	}
	c2 := &Col{
		Num:   c.Num + 1,
		Left:  c1.Right,
		Right: c.Right,
	}
	return Cols{c1, c2}
}

func prime(img *image.NRGBA) (*Primed, error) {
	row0, err := findRow0(img)
	if err != nil {
		return nil, err
	}

	col0, err := findCol0(img, row0)
	if err != nil {
		return nil, err
	}

	rows := findRows(img, row0)
	cols := findCols(img, row0, col0)

	clean(img, rows, cols)

	primed := draw(img, rows, cols)
	primedb, err := encode(primed)
	if err != nil {
		return nil, err
	}

	rows.Shift(-rows[0].Top)
	cols.Shift(-cols[0].Left)

	// first col half width
	cols[0].Right /= 2
	// last col split
	cols = append(cols[:len(cols)-1], cols[len(cols)-1].Split()...)

	return &Primed{
		Data: primedb,
		Skeleton: Skeleton{
			W:    primed.Bounds().Dx(),
			H:    primed.Bounds().Dy(),
			Rows: rows,
			Cols: cols,
		},
	}, nil

}

func findRow0(img image.Image) (int, error) {
	ymax := img.Bounds().Max.Y
	for y := 0; y < ymax; y++ {
		if img.At(0, y) != white {
			return y, nil
		}
	}
	return 0, errors.New("first row not found")
}

func findCol0(img image.Image, row0 int) (int, error) {
	xmax := img.Bounds().Max.X
	for x := 3; x < xmax; x++ {
		if img.At(x, row0) == black {
			return x, nil
		}
	}
	return 0, errors.New("first col not found")
}

func findRows(img image.Image, row0 int) Rows {
	const maxHeight = 50

	max := img.Bounds().Max
	xmax, ymax := max.X-5, max.Y

	var rows Rows
	curr := &Row{
		Num: 0,
		Top: row0,
	}

	for y := row0 + 1; y < ymax; y++ {
		if img.At(xmax, y) != black {
			if y-curr.Top > maxHeight {
				break
			}
			continue
		}

		curr.Bottom = y
		rows = append(rows, curr)
		curr = &Row{
			Num: len(rows),
			Top: y,
		}
	}

	return rows
}

func findCols(img image.Image, row0 int, col0 int) Cols {
	xmax := img.Bounds().Max.X

	var cols Cols
	curr := &Col{
		Num:  0,
		Left: col0,
	}

	for x := col0 + 1; x < xmax; x++ {
		if !isCol(img, x, row0) {
			continue
		}

		curr.Right = x
		cols = append(cols, curr)
		if len(cols) == 3 {
			break
		}

		curr = &Col{
			Num:  len(cols),
			Left: x,
		}
	}

	return cols
}

func isCol(img image.Image, x int, y int) bool {
	for i := 0; i < 5; i++ {
		if img.At(x+i, y) != black {
			return false
		}
		if img.At(x-i, y) != black {
			return false
		}
		if img.At(x, y+i) != black {
			return false
		}
		if img.At(x, y-i) != black {
			return false
		}
	}

	return true
}

func clean(img *image.NRGBA, rows Rows, cols Cols) {
	max := img.Bounds().Max
	xmax, ymax := max.X, max.Y
	for _, r := range rows {
		for x := 0; x < xmax; x++ {
			img.Set(x, r.Top, white)
		}
	}
	for _, c := range cols {
		for y := 0; y < ymax; y++ {
			img.Set(c.Right, y, white)
		}
	}
}

func draw(img *image.NRGBA, rows Rows, cols Cols) image.Image {
	rect := image.Rect(
		cols[0].Left,
		rows[0].Top,
		cols[len(cols)-1].Right,
		rows[len(rows)-1].Bottom,
	)

	dest := image.NewRGBA(rect)
	g := gift.New(
		gift.Crop(rect),
		gift.Grayscale(),
	)
	g.Draw(dest, img)
	return dest
}

func encode(img image.Image) ([]byte, error) {
	var b bytes.Buffer
	err := png.Encode(&b, img)
	if err != nil {
		panic(err)
	}
	return b.Bytes(), nil
}
