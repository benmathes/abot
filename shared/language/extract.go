package language

import (
	"database/sql"
	"encoding/xml"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/itsabot/abot/shared/datatypes"
	"github.com/itsabot/abot/shared/helpers/address"
	"github.com/itsabot/abot/core/log"
	"github.com/jmoiron/sqlx"
)

var regexCurrency = regexp.MustCompile(`\d+\.?\d*`)
var regexNum = regexp.MustCompile(`\d+`)
var regexNonWords = regexp.MustCompile(`[^\w\s]`)

// ExtractCurrency returns a pointer to a string to allow a user a simple check
// to see if currency text was found. If the response is nil, no currency was
// found. This API design also maintains consistency when we want to extract and
// return a struct (which should be returned as a pointer).
func ExtractCurrency(s string) sql.NullInt64 {
	log.Debug("extracting currency")
	n := sql.NullInt64{
		Int64: 0,
		Valid: false,
	}
	s = regexCurrency.FindString(s)
	if len(s) == 0 {
		return n
	}
	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return n
	}
	log.Debug("found value", val)
	n.Int64 = int64(val * 100)
	n.Valid = true
	return n
}

// ExtractYesNo determines whether a string (typically a sentence sent by a
// user to Abot) contains a Yes or No response. This is useful for plugins to
// determine a user's answer to a Yes/No question.
//
// TODO should be converted to return a *bool for consistency with the rest of
// the Extract API.
func ExtractYesNo(s string) sql.NullBool {
	ss := strings.Fields(strings.ToLower(s))
	for _, w := range ss {
		w = strings.TrimRight(w, " .,;:!?'\"")
		if yes[w] {
			return sql.NullBool{
				Bool:  true,
				Valid: true,
			}
		}
		if no[w] {
			return sql.NullBool{
				Bool:  false,
				Valid: true,
			}
		}
	}
	return sql.NullBool{
		Bool:  false,
		Valid: false,
	}
}

// ExtractAddress will return an address from a user's message, whether it's a
// labeled address (e.g. "home", "office"), or a full U.S. address (e.g. 100
// Penn St., CA 90000)
func ExtractAddress(db *sqlx.DB, u *dt.User, s string) (*dt.Address, bool,
	error) {
	addr, err := address.Parse(s)
	if err != nil {
		// check DB for historical information associated with that user
		log.Debug("fetching historical address")
		addr, err = u.GetAddress(db, s)
		if err != nil {
			return nil, false, err
		}
		return addr, true, nil
	}
	type addr2S struct {
		XMLName  xml.Name `xml:"Address"`
		ID       string   `xml:"ID,attr"`
		FirmName string
		Address1 string
		Address2 string
		City     string
		State    string
		Zip5     string
		Zip4     string
	}
	addr2Stmp := addr2S{
		ID:       "0",
		Address1: addr.Line2,
		Address2: addr.Line1,
		City:     addr.City,
		State:    addr.State,
		Zip5:     addr.Zip5,
		Zip4:     addr.Zip4,
	}
	if len(addr.Zip) > 0 {
		addr2Stmp.Zip5 = addr.Zip[:5]
	}
	if len(addr.Zip) > 5 {
		addr2Stmp.Zip4 = addr.Zip[5:]
	}
	addrS := struct {
		XMLName    xml.Name `xml:"AddressValidateRequest"`
		USPSUserID string   `xml:"USERID,attr"`
		Address    addr2S
	}{
		USPSUserID: os.Getenv("USPS_USER_ID"),
		Address:    addr2Stmp,
	}
	xmlAddr, err := xml.Marshal(addrS)
	if err != nil {
		return nil, false, err
	}
	log.Debug(string(xmlAddr))
	ul := "https://secure.shippingapis.com/ShippingAPI.dll?API=Verify&XML="
	ul += url.QueryEscape(string(xmlAddr))
	response, err := http.Get(ul)
	if err != nil {
		return nil, false, err
	}
	contents, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return nil, false, err
	}
	if err = response.Body.Close(); err != nil {
		return nil, false, err
	}
	resp := struct {
		XMLName    xml.Name `xml:"AddressValidateResponse"`
		USPSUserID string   `xml:"USERID,attr"`
		Address    addr2S
	}{
		USPSUserID: os.Getenv("USPS_USER_ID"),
		Address:    addr2Stmp,
	}
	if err = xml.Unmarshal(contents, &resp); err != nil {
		log.Debug("USPS response", string(contents))
		return nil, false, err
	}
	a := dt.Address{
		Name:  resp.Address.FirmName,
		Line1: resp.Address.Address2,
		Line2: resp.Address.Address1,
		City:  resp.Address.City,
		State: resp.Address.State,
		Zip5:  resp.Address.Zip5,
		Zip4:  resp.Address.Zip4,
	}
	if len(resp.Address.Zip4) > 0 {
		a.Zip = resp.Address.Zip5 + "-" + resp.Address.Zip4
	} else {
		a.Zip = resp.Address.Zip5
	}
	return &a, false, nil
}

// ExtractCount returns a number from a user's message, useful in situations
// like:
//	Ava>  How many would you like to buy?
//	User> Order 5.
//
// TODO this should return an *int64 to maintain consistency with the Extract
// API.
func ExtractCount(s string) sql.NullInt64 {
	n := sql.NullInt64{
		Int64: 0,
		Valid: false,
	}
	s = regexNum.FindString(s)
	if len(s) == 0 {
		return n
	}
	val, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return n
	}
	n.Int64 = int64(val)
	n.Valid = true
	return n
}

// ExtractCities efficiently from a user's message.
func ExtractCities(db *sqlx.DB, in *dt.Msg) ([]dt.City, error) {
	// Interface type is used to expand the args in db.Select below.
	// Although we're only storing strings, []string{} doesn't work.
	var args []interface{}

	// Look for "at", "in", "on" prepositions to signal that locations
	// follow, skipping everything before
	var start int
	for i := range in.Stems {
		switch in.Stems[i] {
		case "at", "in", "on":
			start = i
			break
		}
	}

	// Prepare sentence for iteration
	tmp := regexNonWords.ReplaceAllString(in.Sentence, "")
	words := strings.Fields(tmp)

	// Iterate through words and bigrams to assemble a DB query
	for i := start; i < len(words); i++ {
		args = append(args, words[i])
	}
	bgs := bigrams(words, start)
	for i := 0; i < len(bgs); i++ {
		args = append(args, bgs[i])
	}

	cities := []dt.City{}
	q := `SELECT name, countrycode FROM cities WHERE countrycode='US' AND name IN (?) ORDER BY LENGTH(name) DESC`
	query, arguments, err := sqlx.In(q, args)
	query = db.Rebind(query)
	rows, err := db.Query(query, arguments...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		city := dt.City{}
		if err = rows.Scan(&city.Name, &city.CountryCode); err != nil {
			return nil, err
		}
		cities = append(cities, city)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	return cities, nil
}

func bigrams(words []string, startIndex int) (bigrams []string) {
	for i := startIndex; i < len(words)-1; i++ {
		bigrams = append(bigrams, words[i]+" "+words[i+1])
	}
	return bigrams
}
