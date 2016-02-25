package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	log "github.com/Sirupsen/logrus"
	"github.com/garyburd/go-oauth/oauth"
	"github.com/itsabot/abot/shared/datatypes"
	"github.com/itsabot/abot/shared/knowledge"
	"github.com/itsabot/abot/shared/language"
	"github.com/itsabot/abot/shared/nlp"
	"github.com/itsabot/abot/shared/pkg"
	"github.com/jmoiron/sqlx"
)

type Mechanic string

type client struct {
	client oauth.Client
	token  oauth.Credentials
}

type yelpResp struct {
	Businesses []struct {
		Name         string
		ImageURL     string `json:"image_url"`
		MobileURL    string `json:"mobile_url"`
		DisplayPhone string `json:"display_phone"`
		Distance     int
		Rating       float64
		Location     struct {
			City           string
			DisplayAddress []string `json:"display_address"`
		}
	}
}

var ErrNoBusinesses = errors.New("no businesses")

var c client
var p *pkg.Pkg
var db *sqlx.DB
var l *log.Entry

func main() {
	var coreaddr string
	flag.StringVar(&coreaddr, "coreaddr", "",
		"Port used to communicate with Abot.")
	flag.Parse()
	l = log.WithFields(log.Fields{"pkg": "mechanic"})

	c.client.Credentials.Token = os.Getenv("YELP_CONSUMER_KEY")
	c.client.Credentials.Secret = os.Getenv("YELP_CONSUMER_SECRET")
	c.token.Token = os.Getenv("YELP_TOKEN")
	c.token.Secret = os.Getenv("YELP_TOKEN_SECRET")
	var err error
	db, err = pkg.ConnectDB()
	if err != nil {
		l.Fatalln(err)
	}
	trigger := &nlp.StructuredInput{
		Commands: language.Join(
			language.Recommend(),
			language.Broken(),
			language.Repair(),
			[]string{"tow"},
		),
		Objects: language.Join(
			language.Vehicles(),
			language.AutomotiveBrands(),
		),
	}
	p, err = pkg.NewPackage("mechanic", coreaddr, trigger)
	if err != nil {
		l.Fatalln("building", err)
	}
	mechanic := new(Mechanic)
	if err := p.Register(mechanic); err != nil {
		l.Fatalln("registering", err)
	}
}

func (pt *Mechanic) Run(m *dt.Msg, resp *string) error {
	m.State = map[string]interface{}{
		"query":      "",
		"location":   "",
		"offset":     float64(0),
		"businesses": []interface{}{},
		"warranty":   "",
		"preference": "",
		"brand":      "",
	}
	si := m.StructuredInput
	query := ""
	for _, o := range si.Objects {
		for _, b := range language.AutomotiveBrands() {
			if strings.ToLower(o) == b {
				m.State["brand"] = b
				break
			}
		}
		query += o + " "
	}
	m.State["query"] = query
	// TODO move to task
	loc, question, err := knowledge.GetLocation(db, m.User)
	if err != nil {
		return err
	}
	if len(question) > 0 {
		if loc != nil && len(loc.Name) > 0 {
			m.State["location"] = loc.Name
		}
		m.Sentence = question
		return nil
	}
	m.State["location"] = loc.Name
	// Occurs in the case of "nearby" or other contextual place terms, where
	// no previous context was available to expand it.
	if len(m.State["location"].(string)) == 0 {
		loc, question, err := knowledge.GetLocation(db, m.User)
		if err != nil {
			return err
		}
		if len(question) > 0 {
			if loc != nil && len(loc.Name) > 0 {
				m.State["location"] = loc.Name
			}
			*resp = question
			return nil
		}
		m.State["location"] = loc.Name
	}
	if err := pt.searchYelp(m, resp); err != nil {
		l.WithField("fn", "searchYelp").Errorln(err)
	}
	return nil
}

func (pt *Mechanic) FollowUp(m *dt.Msg, resp *string) error {
	// First we handle dialog, filling out the user's location
	if m.State["location"] == "" {
		loc := m.Sentence
		if len(loc) > 0 {
			m.State["location"] = loc
			*resp = "Ok. I can help you. " +
				"What kind of car do you drive?"
		}
		return nil
	}

	// Check the automotive brand
	if m.State["brand"] == "" {
		var brand string
		tmp := m.StructuredInput.Objects
	Loop:
		for _, w1 := range language.AutomotiveBrands() {
			for _, w2 := range tmp {
				if w1 == strings.ToLower(w2) {
					brand = w2
					break Loop
				}
			}
		}
		if len(brand) > 0 {
			m.State["brand"] = brand
			*resp = "Is your car still in warranty?"
		}
		return nil
	}

	// Check warranty information
	if m.State["warranty"] == "" {
		warr := m.Sentence
		if language.Yes(warr) {
			m.State["warranty"] = "yes"
			m.State["preference"] = "dealer"
			if err := pt.searchYelp(m, resp); err != nil {
				l.WithField("fn", "searchYelp").Errorln(err)
			}
		} else if language.No(warr) {
			m.State["warranty"] = "no"
			*resp = "Do you prefer the dealership or a recommended mechanic?"
		}
		return nil
	}

	// Does the user prefer dealerships or mechanics?
	if m.State["preference"] == "" {
		words := strings.Fields(m.Sentence)
		for _, w := range words {
			if w == "dealer" || w == "dealers" {
				m.State["preference"] = "dealer"
				break
			} else if w == "mechanic" || w == "mechanics" {
				m.State["preference"] = "mechanic"
				break
			}
		}
		if m.State["preference"] != "" {
			if err := pt.searchYelp(m, resp); err != nil {
				l.WithField("fn", "searchYelp").Errorln(err)
			}
		}
		return nil
	}

	// If no businesses are returned inform the user now
	if m.State["businesses"] != nil &&
		len(m.State["businesses"].([]interface{})) == 0 {
		*resp = "I couldn't find anything like that"
		return nil
	}

	// Responses were returned, and the user has asked this package an
	// additional query. Handle the query by keyword
	words := strings.Fields(m.Sentence)
	offI := int(m.State["offset"].(float64))
	var s string
	for _, w := range words {
		w = strings.TrimRight(w, ").,;?!:")
		switch strings.ToLower(w) {
		case "rated", "rating", "review", "recommend", "recommended":
			s = fmt.Sprintf("It has a %s star review on Yelp",
				getRating(m, offI))
			*resp = s
		case "number", "phone":
			s = getPhone(m, offI)
			*resp = s
		case "call":
			s = fmt.Sprintf("Try this one: %s", getPhone(m, offI))
			*resp = s
		case "information", "info":
			s = fmt.Sprintf("Here's some more info: %s",
				getURL(m, offI))
			*resp = s
		case "where", "location", "address", "direction", "directions",
			"addr":
			s = fmt.Sprintf("It's at %s", getAddress(m, offI))
			*resp = s
		case "pictures", "pic", "pics":
			s = fmt.Sprintf("I found some pics here: %s",
				getURL(m, offI))
			*resp = s
		case "not", "else", "no", "anything", "something":
			m.State["offset"] = float64(offI + 1)
			if err := pt.searchYelp(m, resp); err != nil {
				l.WithField("fn", "searchYelp").Errorln(err)
			}
		// TODO perhaps handle this case and "thanks" at the Abot level?
		// with bayesian classification
		case "good", "great", "yes", "perfect":
			// TODO feed into learning engine
			*resp = language.Positive()
		case "thanks", "thank":
			*resp = language.Welcome()
		}
		if len(m.Sentence) > 0 {
			return nil
		}
	}
	return nil
}

func getRating(r *dt.Msg, offset int) string {
	businesses := r.State["businesses"].([]interface{})
	firstBusiness := businesses[offset].(map[string]interface{})
	return fmt.Sprintf("%.1f", firstBusiness["Rating"].(float64))
}

func getURL(r *dt.Msg, offset int) string {
	businesses := r.State["businesses"].([]interface{})
	firstBusiness := businesses[offset].(map[string]interface{})
	return firstBusiness["mobile_url"].(string)
}

func getPhone(r *dt.Msg, offset int) string {
	businesses := r.State["businesses"].([]interface{})
	firstBusiness := businesses[offset].(map[string]interface{})
	return firstBusiness["display_phone"].(string)
}

func getAddress(r *dt.Msg, offset int) string {
	businesses := r.State["businesses"].([]interface{})
	firstBusiness := businesses[offset].(map[string]interface{})
	location := firstBusiness["Location"].(map[string]interface{})
	dispAddr := location["display_address"].([]interface{})
	if len(dispAddr) > 1 {
		str1 := dispAddr[0].(string)
		str2 := dispAddr[1].(string)
		return fmt.Sprintf("%s in %s", str1, str2)
	}
	return dispAddr[0].(string)
}

func (c *client) get(urlStr string, params url.Values, v interface{}) error {
	resp, err := c.client.Get(nil, &c.token, urlStr, params)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		l.WithField("fn", "get").Errorln(resp)
		return fmt.Errorf("yelp status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func (pt *Mechanic) searchYelp(m *dt.Msg, resp *string) error {
	q := m.State["query"].(string)
	loc := m.State["location"].(string)
	pref := m.State["preference"].(string)
	brand := m.State["brand"].(string)
	offset := m.State["offset"].(float64)
	if brand != "" {
		q = fmt.Sprintf("%s %s", brand, pref)
	} else {
		q = fmt.Sprintf("%s mechanic", q)
	}
	l.WithFields(log.Fields{
		"q":      q,
		"loc":    loc,
		"offset": offset,
	}).Infoln("searching yelp")
	form := url.Values{
		"term":     {q},
		"location": {loc},
		"limit":    {fmt.Sprintf("%.0f", offset+1)},
	}
	var data yelpResp
	err := c.get("http://api.yelp.com/v2/search", form, &data)
	if err != nil {
		*resp = "I can't find that for you now. Let's try again later."
		return err
	}
	m.State["businesses"] = data.Businesses
	if len(data.Businesses) == 0 {
		*resp = "I couldn't find any places like that nearby."
		return nil
	}
	offI := int(offset)
	if len(data.Businesses) <= offI {
		*resp = "That's all I could find."
		return nil
	}
	b := data.Businesses[offI]
	addr := ""
	if len(b.Location.DisplayAddress) > 0 {
		addr = b.Location.DisplayAddress[0]
	}
	if offI == 0 {
		*resp = "Ok. How does this place look? " + b.Name +
			" at " + addr
	} else {
		*resp = fmt.Sprintf("What about %s instead?", b.Name)
	}
	return nil
}
