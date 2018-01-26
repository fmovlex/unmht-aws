package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/rekognition"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
)

type Magic struct {
	Problems []string
	Fixes    []string
}

func analyze(s *session.Session, msgID string, pngr io.Reader) (*Magic, error) {
	img, err := png.Decode(pngr)
	if err != nil {
		return nil, fmt.Errorf("failed to decode png: %v", err)
	}

	nimg, ok := img.(*image.NRGBA)
	if !ok {
		return nil, fmt.Errorf("failed to cast NRGBA: %v", img)
	}

	primed, err := prime(nimg)
	if err != nil {
		return nil, fmt.Errorf("failed to prime image: %v", err)
	}

	rc := rekognition.New(s)
	out, err := rc.DetectText(&rekognition.DetectTextInput{
		Image: &rekognition.Image{Bytes: primed.Data},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to call rekognition api: %v", err)
	}

	entries, err := scan(out, primed.Skeleton)
	if err != nil {
		uploadDebug(s, msgID, primed.Data, out)
		return nil, fmt.Errorf("failed to parse entries: %v. debug logs: %s", err, msgID)
	}

	return domagic(entries), nil
}

func uploadDebug(s *session.Session, msgID string, img []byte, rekout *rekognition.DetectTextOutput) {
	s3c := s3manager.NewUploader(s)

	imgKey := fmt.Sprintf("%s-debug-img", msgID)
	s3c.Upload(&s3manager.UploadInput{
		Bucket: &bucket,
		Key:    &imgKey,
		Body:   bytes.NewReader(img),
	})

	var rekJSON bytes.Buffer
	err := json.NewEncoder(&rekJSON).Encode(rekout)
	if err == nil {
		rekKey := fmt.Sprintf("%s-debug-rek", msgID)
		s3c.Upload(&s3manager.UploadInput{
			Bucket: &bucket,
			Key:    &rekKey,
			Body:   &rekJSON,
		})
	}
}

type Box struct {
	Top  int
	Left int
	Text string
}

type header int

const (
	DateHeader header = iota
	ActivityHeader
	InHeader
	OutHeader
)

func center(box *rekognition.BoundingBox, sk Skeleton) image.Point {
	w, h := float64(sk.W), float64(sk.H)
	xmid := *box.Left*w + *box.Width*w/2
	ymid := *box.Top*h + *box.Height*h/2
	return image.Point{X: int(xmid), Y: int(ymid)}
}

func scan(out *rekognition.DetectTextOutput, sk Skeleton) ([]Entry, error) {
	var entries = make([]Entry, len(sk.Rows))

	dts := append([]*rekognition.TextDetection{}, out.TextDetections...)
	sort.Slice(dts, func(i, j int) bool {
		return *dts[i].Geometry.BoundingBox.Left < *dts[j].Geometry.BoundingBox.Left
	})

	for _, d := range dts {
		if d.Type == nil || *d.Type != "WORD" {
			continue
		}

		box := d.Geometry.BoundingBox
		center := center(box, sk)

		c, err := sk.Cols.Find(center.X)
		if err != nil {
			continue
		}

		r, err := sk.Rows.Find(center.Y)
		if err != nil {
			continue
		}

		txt := *d.DetectedText
		e := &entries[r]

		switch header(c) {
		case DateHeader:
			date, err := parseDate(txt)
			if err != nil {
				return nil, err
			}
			e.date = date
		case ActivityHeader:
			e.activity += txt
		case InHeader:
			t, err := parseTime(e.date.Format(dateFormat), txt)
			if err != nil {
				return nil, err
			}
			e.in = t
		case OutHeader:
			t, err := parseTime(e.date.Format(dateFormat), txt)
			if err != nil {
				return nil, err
			}
			e.out = t
		}
	}

	return entries, nil
}

func parseDate(s string) (time.Time, error) {
	var date time.Time
	var clean string

	switch len(s) {
	case 9:
		clean = fmt.Sprintf("%s%s%s", s[:2], s[2:4], s[5:])
	case 10:
		clean = fmt.Sprintf("%s%s%s", s[:2], s[3:5], s[6:])
	case 11:
		undup := strings.Replace(s, "1/", "1", 1)
		if len(undup) == 10 {
			return parseDate(undup)
		}
		fallthrough
	default:
		return date, fmt.Errorf("unexpected date format: %v", s)
	}

	date, err := time.Parse(dateFormat, clean)
	if err != nil {
		return date, fmt.Errorf("failed to parse date: %v", err)
	}

	return date, nil
}

func parseTime(sdate string, stime string) (*time.Time, error) {
	s := sdate + stime
	t, err := time.Parse(timeFormat, s)
	if err != nil {
		t, err = time.Parse(timeFormat2, s)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

const (
	prettyDate  = "02/01"
	prettyTime  = "15:04"
	dateFormat  = "02012006"
	timeFormat  = dateFormat + "15.04"
	timeFormat2 = dateFormat + "1504"
)

type Entry struct {
	date     time.Time
	activity string
	in       *time.Time
	out      *time.Time
}

func average(entries []Entry, extractor func(Entry) *time.Time, def time.Duration) time.Duration {
	var total time.Duration
	var count int64 = 0
	for _, e := range entries {
		v := extractor(e)
		if v == nil {
			continue
		}

		d := time.Duration(v.Hour())*time.Hour + time.Duration(v.Minute())*time.Minute
		count++
		total += d
	}

	if count == 0 {
		return def
	}

	avg := time.Duration(total.Nanoseconds()/count) * time.Nanosecond
	return avg.Round(15 * time.Minute)
}

func averageIn(entries []Entry) time.Duration {
	return average(entries, func(e Entry) *time.Time {
		return e.in
	}, 9*time.Hour)
}

func averageOut(entries []Entry) time.Duration {
	return average(entries, func(e Entry) *time.Time {
		return e.out
	}, 17*time.Hour)
}

func domagic(entries []Entry) *Magic {
	avgIn := averageIn(entries)
	avgOut := averageOut(entries)

	var problems []string
	var fixes []string

	for _, e := range entries {
		if e.activity == "" {
			continue
		}
		datefmt := e.date.Format(prettyDate)
		switch e.activity {
		case "UnclosedDay":
			p := fmt.Sprintf("%s: Unclosed day", datefmt)
			problems = append(problems, p)
			fix, err := fixEntry(e, avgIn, avgOut)
			if err != nil {
				log.Printf("failed to generate fix text: %v\n", err)
				continue
			}
			fixes = append(fixes, fix)
		case "Vacation/Una":
			p := fmt.Sprintf("%s: Missing report", datefmt)
			problems = append(problems, p)
		default:
			p := fmt.Sprintf("%s: %s", datefmt, e.activity)
			problems = append(problems, p)
		}
	}

	return &Magic{
		Problems: problems,
		Fixes:    fixes,
	}
}

func fixEntry(e Entry, avgIn time.Duration, avgOut time.Duration) (string, error) {
	if e.in == nil && e.out == nil {
		return "", errors.New("both times are empty")
	}

	if e.in != nil && e.out != nil {
		return "", errors.New("both times are non-empty")
	}

	pdate := e.date.Format(prettyDate)

	var pin, pout string
	if e.in != nil {
		pin = e.in.Format(prettyTime)
		pout = e.date.Add(avgOut).Format(prettyTime)
	} else {
		pin = e.date.Add(avgIn).Format(prettyTime)
		pout = e.out.Format(prettyTime)
	}

	txt := fmt.Sprintf("%s: arrived at %s, left at %s", pdate, pin, pout)
	return txt, nil
}
