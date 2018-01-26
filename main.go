package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"mime/multipart"
	"net/mail"
	"os"
	"strings"
	"text/template"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/ses"
)

var bucket string
var domains []string
var whitelist []string

func main() {
	bucket = os.Getenv("UNMHT_BUCKET")
	domains = strings.Split(os.Getenv("UNMHT_EMAILS"), ",")
	whitelist = strings.Split(os.Getenv("UNMHT_SENDER_WHITELIST"), ",")

	lambda.Start(handler)
}

type SESNotification struct {
	Records []struct {
		SES struct {
			Mail struct {
				Source      string   `json:"source"`
				Destination []string `json:"destination"`
				MessageID   string   `json:"messageId"`
			} `json:"mail"`
		} `json:"ses"`
	} `json:"Records"`
}

func handler(in SESNotification) {
	sesMail := in.Records[0].SES.Mail

	if err := checkWhitelist(sesMail.Source); err != nil {
		log.Println(err)
		return
	}

	unmhtRecipient, err := findMe(sesMail.Destination)
	if err != nil {
		log.Println(err)
		return
	}

	s, err := session.NewSession()
	if err != nil {
		log.Println(err)
		return
	}

	body, err := getMail(s, sesMail.MessageID)
	if err != nil {
		log.Printf("failed to get mail from s3: %v", err)
		return
	}
	defer body.Close()

	msg, err := mail.ReadMessage(body)
	if err != nil {
		log.Printf("failed to read mail message: %v", err)
		return
	}

	mht64, err := extractMHT(msg)
	if err != nil {
		log.Printf("failed to extract mht: %v", err)
		return
	}

	mht := base64.NewDecoder(base64.StdEncoding, mht64)

	mhtMsg, err := mail.ReadMessage(mht)
	if err != nil {
		log.Printf("failed to read mail message inside mht: %v", err)
		return
	}

	png64, err := extractPNG(mhtMsg)
	if err != nil {
		log.Printf("failed to extract png: %v", err)
		return
	}
	png64b, _ := ioutil.ReadAll(png64)

	pngr := base64.NewDecoder(base64.StdEncoding, bytes.NewReader(png64b))
	analytics := analyticsString(s, sesMail.MessageID, pngr)

	rep, err := runTmpl(replyTmpl, replyData{
		From:      unmhtRecipient,
		To:        sesMail.Source,
		Subject:   msg.Header.Get("Subject"),
		InReplyTo: msg.Header.Get("Message-ID"),
		Analytics: analytics,
		PNGStr:    string(png64b),
	})
	if err != nil {
		log.Printf("failed to render reply template: %v", err)
		return
	}

	err = sendReply(s, rep)
	if err != nil {
		log.Printf("failed to send reply: %v", err)
	}
}

func analyticsString(s *session.Session, msgID string, pngr io.Reader) string {
	const nothing = "no analytics available."

	magic, err := analyze(s, msgID, pngr)
	if err != nil {
		log.Printf("failed to get analytics: %v\n", err)
		return nothing
	}

	if len(magic.Problems) == 0 {
		return "things look ok."
	}

	str, err := runTmpl(analyticsTmpl, magic)
	if err != nil {
		log.Printf("failed to render analytics: %v\n", err)
		return nothing
	}

	return str
}

func checkWhitelist(source string) error {
	whiteset := map[string]bool{}
	for _, w := range whitelist {
		whiteset[w] = true
	}

	addr, err := mail.ParseAddress(source)
	if err != nil {
		return fmt.Errorf("failed to parse source address: %v", err)
	}

	at := strings.LastIndex(addr.Address, "@")
	domain := addr.Address[at+1:]

	if _, ok := whiteset[domain]; !ok {
		return fmt.Errorf("sender not in whitelist: %v [%v]", source, domain)
	}

	return nil
}

func findMe(dest []string) (string, error) {
	domset := map[string]bool{}
	for _, d := range domains {
		domset[d] = true
	}

	for _, d := range dest {
		if _, ok := domset[d]; ok {
			return d, nil
		}
	}

	return "", fmt.Errorf("self-domain not found in destinations: %v", dest)
}

func getMail(s *session.Session, msgID string) (io.ReadCloser, error) {
	s3c := s3.New(s)
	obj, err := s3c.GetObject(&s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &msgID,
	})

	if err != nil {
		return nil, err
	}

	return obj.Body, nil
}

func extractMHT(msg *mail.Message) (io.Reader, error) {
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		return nil, fmt.Errorf("failed to parse media type: %v", err)
	}
	if mt != "multipart/mixed" {
		return nil, fmt.Errorf("expected multipart/mixed, but got %s", mt)
	}

	boundary := params["boundary"]
	reader := multipart.NewReader(msg.Body, boundary)

	var found io.Reader
	part, err := reader.NextPart()
	for i := 0; err == nil && i < 20; i++ {
		cd := part.Header.Get("Content-Disposition")
		ct := part.Header.Get("Content-Type")
		cte := part.Header.Get("Content-Transfer-Encoding")

		if strings.Contains(cd, ".mht") && strings.HasPrefix(ct, "application/octet-stream") && cte == "base64" {
			found = part
			break
		}
		part, err = reader.NextPart()
	}

	if found == nil {
		return nil, errors.New("couldn't find an attachment - not a timetable")
	}

	return found, nil
}

func extractPNG(msg *mail.Message) (io.Reader, error) {
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		return nil, fmt.Errorf("failed to parse media type: %v", err)
	}
	if mt != "multipart/related" {
		return nil, fmt.Errorf("expected multipart/related, but got %s", mt)
	}

	boundary := params["boundary"]
	reader := multipart.NewReader(msg.Body, boundary)

	var found io.Reader
	part, err := reader.NextPart()
	for i := 0; err == nil && i < 20; i++ {
		ct := part.Header.Get("Content-Type")
		cte := part.Header.Get("Content-Transfer-Encoding")
		if ct == "image/png" && cte == "base64" {
			found = part
			break
		}
		part, err = reader.NextPart()
	}

	if found == nil {
		return nil, errors.New("couldn't find an encoded png - not a timetable")
	}

	return found, nil
}

func sendReply(s *session.Session, rep string) error {
	sesc := ses.New(s)
	_, err := sesc.SendRawEmail(&ses.SendRawEmailInput{
		RawMessage: &ses.RawMessage{Data: []byte(rep)},
	})
	return err
}

type replyData struct {
	From      string
	To        string
	Subject   string
	InReplyTo string
	Analytics string
	PNGStr    string
}

func runTmpl(tmpl *template.Template, data interface{}) (string, error) {
	var b bytes.Buffer
	err := tmpl.Execute(&b, data)
	if err != nil {
		return "", err
	}
	return b.String(), nil
}

var replyTmpl = template.Must(template.New("reply").Parse(`Content-Type: multipart/mixed; boundary="bo_un_da_ry"
MIME-Version: 1.0
From: {{.From}}
To: {{.To}}
Subject: RE: {{.Subject}}
References: {{.InReplyTo}}
In-Reply-To: {{.InReplyTo}}

--bo_un_da_ry
Content-Type: text/plain; charset="UTF-8"
MIME-Version: 1.0
Content-Transfer-Encoding: 7bit

Here's a fresh unmht for you buddy.
{{.Analytics}}

--bo_un_da_ry
Content-Type: image/png
MIME-Version: 1.0
Content-Transfer-Encoding: base64
Content-Disposition: attachment; filename="times.png"

{{.PNGStr}}

--bo_un_da_ry--`))

var analyticsTmpl = template.Must(template.New("analytics").Parse(`
{{- if .Problems }}
Looks like there's a few non-standard days:
{{- range .Problems }}
  - {{.}}
{{- end }}
{{ if .Fixes }}
Here's a quick reply for your partial days:

---------------

Hey,
{{ range .Fixes }}
{{.}}
{{- end }}

Thanks

---------------

{{ end }}
{{ else }}
Everything looks good.
{{ end }}`))
