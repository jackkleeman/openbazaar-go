package api

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	mh "gx/ipfs/QmYDds3421prZgqKbLpEK7T9Aa2eVdQ7o3YarX1LVLdP2J/go-multihash"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"encoding/hex"

	"github.com/OpenBazaar/jsonpb"
	"github.com/OpenBazaar/openbazaar-go/core"
	"github.com/OpenBazaar/openbazaar-go/ipfs"
	"github.com/OpenBazaar/openbazaar-go/pb"
	"github.com/OpenBazaar/openbazaar-go/repo"
	"github.com/OpenBazaar/spvwallet"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	btc "github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/base58"
	"github.com/golang/protobuf/ptypes/timestamp"
	"github.com/ipfs/go-ipfs/core/coreunix"
	ipnspath "github.com/ipfs/go-ipfs/path"
	lockfile "github.com/ipfs/go-ipfs/repo/fsrepo/lock"
	routing "github.com/ipfs/go-ipfs/routing/dht"
	multiaddr "github.com/multiformats/go-multiaddr"
	multihash "github.com/multiformats/go-multihash"
	"golang.org/x/net/context"
)

type JsonAPIConfig struct {
	Headers       map[string][]string
	Enabled       bool
	Cors          *string
	Authenticated bool
	Cookie        http.Cookie
	Username      string
	Password      string
}

type jsonAPIHandler struct {
	config JsonAPIConfig
	node   *core.OpenBazaarNode
}

func newJsonAPIHandler(node *core.OpenBazaarNode, authCookie http.Cookie, config repo.APIConfig) (*jsonAPIHandler, error) {

	i := &jsonAPIHandler{
		config: JsonAPIConfig{
			Enabled:       config.Enabled,
			Cors:          config.CORS,
			Headers:       config.HTTPHeaders,
			Authenticated: config.Authenticated,
			Cookie:        authCookie,
			Username:      config.Username,
			Password:      config.Password,
		},
		node: node,
	}
	return i, nil
}

func (i *jsonAPIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !i.config.Enabled {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "403 - Forbidden")
		return
	}
	if i.config.Cors != nil {
		w.Header().Set("Access-Control-Allow-Origin", *i.config.Cors)
		w.Header().Set("Access-Control-Allow-Methods", "PUT,POST,DELETE")
		w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization")
	}

	for k, v := range i.config.Headers {
		w.Header()[k] = v
	}

	if i.config.Authenticated {
		if i.config.Username == "" || i.config.Password == "" {
			cookie, err := r.Cookie("OpenBazaar_Auth_Cookie")
			if err != nil {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, "403 - Forbidden")
				return
			}
			if i.config.Cookie.Value != cookie.Value {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, "403 - Forbidden")
				return
			}
		} else {
			username, password, ok := r.BasicAuth()
			if !ok || username != i.config.Username || password != i.config.Password {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, "403 - Forbidden")
				return
			}
		}
	}

	// Stop here if its Preflighted OPTIONS request
	if r.Method == "OPTIONS" {
		return
	}
	dump, err := httputil.DumpRequest(r, false)
	if err != nil {
		log.Error("Error reading http request:", err)
	}
	log.Debugf("%s", dump)
	defer func() {
		if r := recover(); r != nil {
			log.Error("A panic occurred in the rest api handler!")
			log.Error(r)
			debug.PrintStack()
		}
	}()

	u, err := url.Parse(r.URL.Path)
	if err != nil {
		panic(err)
	}
	w.Header().Add("Content-Type", "application/json")
	switch r.Method {
	case "GET":
		get(i, u.String(), w, r)
	case "POST":
		post(i, u.String(), w, r)
	case "PUT":
		put(i, u.String(), w, r)
	case "DELETE":
		deleter(i, u.String(), w, r)
	case "PATCH":
		patch(i, u.String(), w, r)
	}
}

func ErrorResponse(w http.ResponseWriter, errorCode int, reason string) {
	type ApiError struct {
		Success bool   `json:"success"`
		Reason  string `json:"reason"`
	}
	reason = strings.Replace(reason, `"`, `'`, -1)
	err := ApiError{false, reason}
	resp, _ := json.MarshalIndent(err, "", "    ")
	w.WriteHeader(errorCode)
	fmt.Fprint(w, string(resp))
}

func (i *jsonAPIHandler) POSTProfile(w http.ResponseWriter, r *http.Request) {

	// If the profile is already set tell them to use PUT
	profilePath := path.Join(i.node.RepoPath, "root", "profile")
	_, ferr := os.Stat(profilePath)
	if !os.IsNotExist(ferr) {
		ErrorResponse(w, http.StatusConflict, "Profile already exists. Use PUT.")
		return
	}

	// Check JSON decoding and add proper indentation
	profile := new(pb.Profile)
	err := jsonpb.Unmarshal(r.Body, profile)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Save to file
	err = i.node.UpdateProfile(profile)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Update followers/following
	err = i.node.UpdateFollow()
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, "File Write Error: "+err.Error())
		return
	}

	// Republish to IPNS
	if err := i.node.SeedNode(); err != nil {
		ErrorResponse(w, http.StatusInternalServerError, "IPNS Error: "+err.Error())
		return
	}

	// Write profile back out as JSON
	m := jsonpb.Marshaler{
		EnumsAsInts:  false,
		EmitDefaults: true,
		Indent:       "    ",
		OrigName:     false,
	}
	out, err := m.MarshalToString(profile)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, out)
	return
}

func (i *jsonAPIHandler) PUTProfile(w http.ResponseWriter, r *http.Request) {

	// If profile is not set tell them to use POST
	profilePath := path.Join(i.node.RepoPath, "root", "profile")
	_, ferr := os.Stat(profilePath)
	if os.IsNotExist(ferr) {
		ErrorResponse(w, http.StatusNotFound, "Profile doesn't exist yet. Use POST.")
		return
	}

	// Check JSON decoding and add proper indentation
	profile := new(pb.Profile)
	err := jsonpb.Unmarshal(r.Body, profile)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Save to file
	err = i.node.UpdateProfile(profile)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Update followers/following
	err = i.node.UpdateFollow()
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Republish to IPNS
	if err := i.node.SeedNode(); err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Return the profile in JSON format
	m := jsonpb.Marshaler{
		EnumsAsInts:  false,
		EmitDefaults: true,
		Indent:       "    ",
		OrigName:     false,
	}
	out, err := m.MarshalToString(profile)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, out)
	return
}

func (i *jsonAPIHandler) PATCHProfile(w http.ResponseWriter, r *http.Request) {
	// If profile is not set tell them to use POST
	profilePath := path.Join(i.node.RepoPath, "root", "profile")
	_, ferr := os.Stat(profilePath)
	if os.IsNotExist(ferr) {
		ErrorResponse(w, http.StatusNotFound, "Profile doesn't exist yet. Use POST.")
		return
	}

	// Read JSON from r.Body and decode into map
	patch := make(map[string]interface{})
	patchBytes, err := ioutil.ReadAll(r.Body)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	err = json.Unmarshal(patchBytes, &patch)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Apply patch
	err = i.node.PatchProfile(patch)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Update followers/following
	err = i.node.UpdateFollow()
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Republish to IPNS
	if err := i.node.SeedNode(); err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	fmt.Fprint(w, `{}`)
	return
}

func (i *jsonAPIHandler) POSTAvatar(w http.ResponseWriter, r *http.Request) {
	type ImgData struct {
		Avatar string `json:"avatar"`
	}
	decoder := json.NewDecoder(r.Body)
	data := new(ImgData)
	err := decoder.Decode(&data)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	hashes, err := i.node.SetAvatarImages(data.Avatar)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Update followers/following
	err = i.node.UpdateFollow()
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := i.node.SeedNode(); err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonHashes, err := json.MarshalIndent(hashes, "", "    ")
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, string(jsonHashes))
	return
}

func (i *jsonAPIHandler) POSTHeader(w http.ResponseWriter, r *http.Request) {
	type ImgData struct {
		Header string `json:"header"`
	}
	decoder := json.NewDecoder(r.Body)
	data := new(ImgData)
	err := decoder.Decode(&data)

	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	hashes, err := i.node.SetHeaderImages(data.Header)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Update followers/following
	err = i.node.UpdateFollow()
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, "File write error: "+err.Error())
		return
	}

	if err := i.node.SeedNode(); err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonHashes, err := json.MarshalIndent(hashes, "", "    ")
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, string(jsonHashes))
	return
}

func (i *jsonAPIHandler) POSTImage(w http.ResponseWriter, r *http.Request) {
	type ImgData struct {
		Filename string `json:"filename"`
		Image    string `json:"image"`
	}
	decoder := json.NewDecoder(r.Body)
	var images []ImgData
	err := decoder.Decode(&images)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	type retImage struct {
		Filename string      `json:"filename"`
		Hashes   core.Images `json:"hashes"`
	}
	var retData []retImage
	for _, img := range images {
		hashes, err := i.node.SetProductImages(img.Image, img.Filename)
		if err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		rtimg := retImage{img.Filename, *hashes}
		retData = append(retData, rtimg)
	}
	jsonHashes, err := json.MarshalIndent(retData, "", "    ")
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, string(jsonHashes))
	return
}

func (i *jsonAPIHandler) POSTListing(w http.ResponseWriter, r *http.Request) {
	ld := new(pb.ListingReqApi)
	err := jsonpb.Unmarshal(r.Body, ld)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// If the listing already exists tell them to use PUT
	listingPath := path.Join(i.node.RepoPath, "root", "listings", ld.Listing.Slug+".json")
	if ld.Listing.Slug != "" {
		_, ferr := os.Stat(listingPath)
		if !os.IsNotExist(ferr) {
			ErrorResponse(w, http.StatusConflict, "Listing already exists. Use PUT.")
			return
		}
	}
	contract, err := i.node.SignListing(ld.Listing)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	listingPath = path.Join(i.node.RepoPath, "root", "listings", contract.VendorListings[0].Slug+".json")
	err = i.node.SetListingInventory(ld.Listing, ld.Inventory)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	f, err := os.Create(listingPath)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	m := jsonpb.Marshaler{
		EnumsAsInts:  false,
		EmitDefaults: false,
		Indent:       "    ",
		OrigName:     false,
	}
	out, err := m.MarshalToString(contract)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	if _, err := f.WriteString(out); err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	err = i.node.UpdateListingIndex(contract)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Update followers/following
	err = i.node.UpdateFollow()
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := i.node.SeedNode(); err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprintf(w, `{"slug": "%s"}`, contract.VendorListings[0].Slug)
	return
}

func (i *jsonAPIHandler) PUTListing(w http.ResponseWriter, r *http.Request) {
	ld := new(pb.ListingReqApi)
	err := jsonpb.Unmarshal(r.Body, ld)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	listingPath := path.Join(i.node.RepoPath, "root", "listings", ld.Listing.Slug+".json")
	_, ferr := os.Stat(listingPath)
	if os.IsNotExist(ferr) {
		ErrorResponse(w, http.StatusNotFound, "Listing not found.")
		return
	}
	contract, err := i.node.SignListing(ld.Listing)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	err = i.node.SetListingInventory(ld.Listing, ld.Inventory)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	f, err := os.Create(listingPath)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	m := jsonpb.Marshaler{
		EnumsAsInts:  false,
		EmitDefaults: false,
		Indent:       "    ",
		OrigName:     false,
	}
	out, err := m.MarshalToString(contract)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	if _, err := f.WriteString(out); err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	err = i.node.UpdateListingIndex(contract)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Update followers/following
	err = i.node.UpdateFollow()
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, "File Write Error: "+err.Error())
		return
	}
	if err := i.node.SeedNode(); err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, `{}`)
	return
}

func (i *jsonAPIHandler) DELETEListing(w http.ResponseWriter, r *http.Request) {
	type deleteReq struct {
		Slug string `json:"slug"`
	}
	decoder := json.NewDecoder(r.Body)
	var req deleteReq
	err := decoder.Decode(&req)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	listingPath := path.Join(i.node.RepoPath, "root", "listings", req.Slug+".json")
	_, ferr := os.Stat(listingPath)
	if os.IsNotExist(ferr) {
		ErrorResponse(w, http.StatusNotFound, "Listing not found.")
		return
	}
	err = i.node.DeleteListing(req.Slug)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	err = i.node.UpdateFollow()
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, "File Write Error: "+err.Error())
		return
	}
	if err := i.node.SeedNode(); err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, `{}`)
	return
}

func (i *jsonAPIHandler) POSTPurchase(w http.ResponseWriter, r *http.Request) {
	decoder := json.NewDecoder(r.Body)
	var data core.PurchaseData
	err := decoder.Decode(&data)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	orderId, paymentAddr, amount, online, err := i.node.Purchase(&data)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	type purchaseReturn struct {
		PaymentAddress string `json:"paymentAddress"`
		Amount         uint64 `json:"amount"`
		VendorOnline   bool   `json:"vendorOnline"`
		OrderId        string `json:"orderId"`
	}
	ret := purchaseReturn{paymentAddr, amount, online, orderId}
	b, err := json.MarshalIndent(ret, "", "    ")
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, string(b))
	return
}

func (i *jsonAPIHandler) GETStatus(w http.ResponseWriter, r *http.Request) {
	_, peerId := path.Split(r.URL.Path)
	status := i.node.GetPeerStatus(peerId)
	fmt.Fprintf(w, `{"status": "%s"}`, status)
}

func (i *jsonAPIHandler) GETPeers(w http.ResponseWriter, r *http.Request) {
	peers, err := ipfs.ConnectedPeers(i.node.Context)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	peerJson, err := json.MarshalIndent(peers, "", "    ")
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, string(peerJson))
}

func (i *jsonAPIHandler) POSTFollow(w http.ResponseWriter, r *http.Request) {
	type PeerId struct {
		ID string `json:"id"`
	}

	decoder := json.NewDecoder(r.Body)
	var pid PeerId
	err := decoder.Decode(&pid)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := i.node.Follow(pid.ID); err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, `{}`)
	return
}

func (i *jsonAPIHandler) POSTUnfollow(w http.ResponseWriter, r *http.Request) {
	type PeerId struct {
		ID string `json:"id"`
	}
	decoder := json.NewDecoder(r.Body)
	var pid PeerId
	err := decoder.Decode(&pid)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := i.node.Unfollow(pid.ID); err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, `{}`)
	return
}

func (i *jsonAPIHandler) GETAddress(w http.ResponseWriter, r *http.Request) {
	addr := i.node.Wallet.CurrentAddress(spvwallet.EXTERNAL)
	fmt.Fprintf(w, `{"address": "%s"}`, addr.EncodeAddress())
}

func (i *jsonAPIHandler) GETMnemonic(w http.ResponseWriter, r *http.Request) {
	mn, err := i.node.Datastore.Config().GetMnemonic()
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprintf(w, `{"mnemonic": "%s"}`, mn)
}

func (i *jsonAPIHandler) GETBalance(w http.ResponseWriter, r *http.Request) {
	confirmed, unconfirmed := i.node.Wallet.Balance()
	fmt.Fprintf(w, `{"confirmed": "%d", "unconfirmed": "%d"}`, int(confirmed), int(unconfirmed))
}

func (i *jsonAPIHandler) POSTSpendCoins(w http.ResponseWriter, r *http.Request) {
	type Send struct {
		Address  string `json:"address"`
		PeerId   string `json:"peerId"`
		Amount   int64  `json:"amount"`
		FeeLevel string `json:"feeLevel"`
	}
	decoder := json.NewDecoder(r.Body)
	var snd Send
	err := decoder.Decode(&snd)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	var feeLevel spvwallet.FeeLevel
	switch strings.ToUpper(snd.FeeLevel) {
	case "PRIORITY":
		feeLevel = spvwallet.PRIOIRTY
	case "NORMAL":
		feeLevel = spvwallet.NORMAL
	case "ECONOMIC":
		feeLevel = spvwallet.ECONOMIC
	}
	if snd.Address == "" {
		peerId := snd.PeerId
		if strings.HasPrefix(peerId, "@") {
			peerId, err = i.node.Resolver.Resolve(peerId)
			if err != nil {
				ErrorResponse(w, http.StatusNotFound, err.Error())
				return
			}
		}
		p, err := ipfs.ResolveThenCat(i.node.Context, ipnspath.FromString(path.Join(peerId, "profile")))
		if err != nil {
			ErrorResponse(w, http.StatusNotFound, err.Error())
			return
		}
		var profile pb.Profile
		err = jsonpb.UnmarshalString(string(p), &profile)
		if err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !profile.AcceptStealth {
			ErrorResponse(w, http.StatusInternalServerError, "Recipeint does not accept stealth payments")
			return
		}
		pubkeyBytes, err := hex.DecodeString(profile.BitcoinPubkey)
		if err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		pubkey, err := btcec.ParsePubKey(pubkeyBytes, btcec.S256())
		if err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := i.node.Wallet.SendStealth(snd.Amount, pubkey, feeLevel); err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		addr, err := btc.DecodeAddress(snd.Address, i.node.Wallet.Params())
		if err != nil {
			ErrorResponse(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := i.node.Wallet.Spend(snd.Amount, addr, feeLevel); err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	fmt.Fprint(w, `{}`)
	return
}

func (i *jsonAPIHandler) GETConfig(w http.ResponseWriter, r *http.Request) {
	testnet := false
	if i.node.Wallet.Params().Name != chaincfg.MainNetParams.Name {
		testnet = true
	}
	fmt.Fprintf(w, `{"guid": "%s", "cryptoCurrency": "%s", "testnet": %t}`, i.node.IpfsNode.Identity.Pretty(), i.node.Wallet.CurrencyCode(), testnet)
}

func (i *jsonAPIHandler) POSTSettings(w http.ResponseWriter, r *http.Request) {
	var settings repo.SettingsData
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(&settings)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	_, err = i.node.Datastore.Settings().Get()
	if err == nil {
		ErrorResponse(w, http.StatusConflict, "Settings is already set. Use PUT.")
		return
	}
	if settings.MisPaymentBuffer == nil {
		i := float32(1)
		settings.MisPaymentBuffer = &i
	}
	err = i.node.Datastore.Settings().Put(settings)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, `{}`)
	return
}

func (i *jsonAPIHandler) PUTSettings(w http.ResponseWriter, r *http.Request) {
	var settings repo.SettingsData
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(&settings)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	_, err = i.node.Datastore.Settings().Get()
	if err != nil {
		ErrorResponse(w, http.StatusNotFound, "Settings is not yet set. Use POST.")
		return
	}
	err = i.node.Datastore.Settings().Put(settings)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, `{}`)
	return
}

func (i *jsonAPIHandler) GETSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := i.node.Datastore.Settings().Get()
	if err != nil {
		ErrorResponse(w, http.StatusNotFound, err.Error())
		return
	}
	settings.Version = &i.node.UserAgent
	settingsJson, err := json.MarshalIndent(&settings, "", "    ")
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, string(settingsJson))
}

func (i *jsonAPIHandler) PATCHSettings(w http.ResponseWriter, r *http.Request) {
	var settings repo.SettingsData
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(&settings)
	if err != nil {
		switch err.Error() {
		case "Not Found":
			ErrorResponse(w, http.StatusNotFound, err.Error())
		default:
			ErrorResponse(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	if settings.StoreModerators != nil {
		if err := i.node.SetModeratorsOnListings(*settings.StoreModerators); err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
		}
		if err := i.node.SeedNode(); err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
		}
	}
	err = i.node.Datastore.Settings().Update(settings)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, `{}`)
}

func (i *jsonAPIHandler) GETClosestPeers(w http.ResponseWriter, r *http.Request) {
	_, peerId := path.Split(r.URL.Path)
	var peerIds []string
	peers, err := ipfs.Query(i.node.Context, peerId)
	if err == nil {
		for _, p := range peers {
			peerIds = append(peerIds, p.Pretty())
		}
	}
	ret, _ := json.MarshalIndent(peerIds, "", "    ")
	if string(ret) == "null" {
		ret = []byte("[]")
	}
	fmt.Fprint(w, string(ret))
}

func (i *jsonAPIHandler) GETExchangeRate(w http.ResponseWriter, r *http.Request) {
	_, currencyCode := path.Split(r.URL.Path)
	if currencyCode == "" || strings.ToLower(currencyCode) == "exchangerate" {
		currencyMap, err := i.node.ExchangeRates.GetAllRates()
		if err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		exchangeRateJson, err := json.MarshalIndent(currencyMap, "", "    ")
		if err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		fmt.Fprint(w, string(exchangeRateJson))

	} else {
		rate, err := i.node.ExchangeRates.GetExchangeRate(strings.ToUpper(currencyCode))
		if err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		fmt.Fprintf(w, `%.2f`, rate)
	}
}

func (i *jsonAPIHandler) GETFollowers(w http.ResponseWriter, r *http.Request) {
	_, peerId := path.Split(r.URL.Path)
	var err error
	if peerId == "" || strings.ToLower(peerId) == "followers" || peerId == i.node.IpfsNode.Identity.Pretty() {
		offset := r.URL.Query().Get("offsetId")
		limit := r.URL.Query().Get("limit")
		if limit == "" {
			limit = "-1"
		}
		l, err := strconv.ParseInt(limit, 10, 32)
		if err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		followers, err := i.node.Datastore.Followers().Get(offset, int(l))
		if err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		ret, _ := json.MarshalIndent(followers, "", "    ")
		if string(ret) == "null" {
			ret = []byte("[]")
		}
		fmt.Fprint(w, string(ret))
	} else {
		if strings.HasPrefix(peerId, "@") {
			peerId, err = i.node.Resolver.Resolve(peerId)
			if err != nil {
				ErrorResponse(w, http.StatusNotFound, err.Error())
				return
			}
		}
		followBytes, err := ipfs.ResolveThenCat(i.node.Context, ipnspath.FromString(path.Join(peerId, "followers")))
		if err != nil {
			ErrorResponse(w, http.StatusNotFound, err.Error())
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=600, immutable")
		fmt.Fprint(w, string(followBytes))
	}
}

func (i *jsonAPIHandler) GETFollowing(w http.ResponseWriter, r *http.Request) {
	_, peerId := path.Split(r.URL.Path)
	var err error
	if peerId == "" || strings.ToLower(peerId) == "following" || peerId == i.node.IpfsNode.Identity.Pretty() {
		offset := r.URL.Query().Get("offsetId")
		limit := r.URL.Query().Get("limit")
		if limit == "" {
			limit = "-1"
		}
		l, err := strconv.ParseInt(limit, 10, 32)
		if err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		followers, err := i.node.Datastore.Following().Get(offset, int(l))
		if err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		ret, _ := json.MarshalIndent(followers, "", "    ")
		if string(ret) == "null" {
			ret = []byte("[]")
		}
		fmt.Fprint(w, string(ret))
	} else {
		if strings.HasPrefix(peerId, "@") {
			peerId, err = i.node.Resolver.Resolve(peerId)
			if err != nil {
				ErrorResponse(w, http.StatusNotFound, err.Error())
				return
			}
		}
		followBytes, err := ipfs.ResolveThenCat(i.node.Context, ipnspath.FromString(path.Join(peerId, "following")))
		if err != nil {
			ErrorResponse(w, http.StatusNotFound, err.Error())
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=600, immutable")
		fmt.Fprint(w, string(followBytes))
	}
}

func (i *jsonAPIHandler) GETInventory(w http.ResponseWriter, r *http.Request) {
	type inv struct {
		Slug     string `json:"slug"`
		Quantity int    `json:"quantity"`
	}
	var invList []inv
	inventory, err := i.node.Datastore.Inventory().GetAll()
	if err != nil {
		fmt.Fprintf(w, `[]`)
	}
	for k, v := range inventory {
		i := inv{k, v}
		invList = append(invList, i)
	}
	ret, _ := json.MarshalIndent(invList, "", "    ")
	if string(ret) == "null" {
		ret = []byte("[]")
	}
	fmt.Fprint(w, string(ret))
	return
}

func (i *jsonAPIHandler) POSTInventory(w http.ResponseWriter, r *http.Request) {
	type inv struct {
		Slug     string `json:"slug"`
		Quantity int    `json:"quantity"`
	}
	decoder := json.NewDecoder(r.Body)
	var invList []inv
	err := decoder.Decode(&invList)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	for _, in := range invList {
		err := i.node.Datastore.Inventory().Put(in.Slug, in.Quantity)
		if err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	fmt.Fprint(w, `{}`)
	return
}

func (i *jsonAPIHandler) PUTModerator(w http.ResponseWriter, r *http.Request) {
	profilePath := path.Join(i.node.RepoPath, "root", "profile")
	_, ferr := os.Stat(profilePath)
	if os.IsNotExist(ferr) {
		ErrorResponse(w, http.StatusConflict, "Profile does not exist. Create one first.")
		return
	}

	// Check JSON decoding and add proper indentation
	moderator := new(pb.Moderator)
	err := jsonpb.Unmarshal(r.Body, moderator)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Save self as moderator
	err = i.node.SetSelfAsModerator(moderator)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Update followers/following
	err = i.node.UpdateFollow()
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, "File Write Error: "+err.Error())
		return
	}

	// Republish to IPNS
	if err := i.node.SeedNode(); err != nil {
		ErrorResponse(w, http.StatusInternalServerError, "IPNS Error: "+err.Error())
		return
	}
	fmt.Fprint(w, "{}")
	return
}

func (i *jsonAPIHandler) DELETEModerator(w http.ResponseWriter, r *http.Request) {
	profile, err := i.node.GetProfile()
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	profile.Moderator = false
	profile.ModInfo = nil
	err = i.node.UpdateProfile(&profile)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Update followers/following
	err = i.node.UpdateFollow()
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Republish to IPNS
	if err := i.node.SeedNode(); err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprintf(w, "{}")
	return
}

func (i *jsonAPIHandler) GETListings(w http.ResponseWriter, r *http.Request) {
	_, peerId := path.Split(r.URL.Path)
	var err error
	if peerId == "" || strings.ToLower(peerId) == "listings" || peerId == i.node.IpfsNode.Identity.Pretty() {
		listingsBytes, err := i.node.GetListings()
		if err != nil {
			ErrorResponse(w, http.StatusNotFound, err.Error())
			return
		}
		fmt.Fprint(w, string(listingsBytes))
	} else {
		if strings.HasPrefix(peerId, "@") {
			peerId, err = i.node.Resolver.Resolve(peerId)
			if err != nil {
				ErrorResponse(w, http.StatusNotFound, err.Error())
				return
			}
		}
		listingsBytes, err := ipfs.ResolveThenCat(i.node.Context, ipnspath.FromString(path.Join(peerId, "listings", "index.json")))
		if err != nil {
			ErrorResponse(w, http.StatusNotFound, err.Error())
			return
		}
		fmt.Fprint(w, string(listingsBytes))
		w.Header().Set("Cache-Control", "public, max-age=600, immutable")
	}
}

func (i *jsonAPIHandler) GETListing(w http.ResponseWriter, r *http.Request) {
	contract := new(pb.RicardianContract)
	inventory := []*pb.Inventory{}
	_, listingID := path.Split(r.URL.Path)
	_, err := mh.FromB58String(listingID)
	if err == nil {
		contract, inventory, err = i.node.GetListingFromHash(listingID)
	} else {
		contract, inventory, err = i.node.GetListingFromSlug(listingID)
	}
	if err != nil {
		ErrorResponse(w, http.StatusNotFound, "Listing not found.")
		return
	}
	m := jsonpb.Marshaler{
		EnumsAsInts:  false,
		EmitDefaults: false,
		Indent:       "    ",
		OrigName:     false,
	}
	resp := new(pb.ListingRespApi)
	resp.Contract = contract
	resp.Inventory = inventory
	out, err := m.MarshalToString(resp)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, string(out))
	return
}

func (i *jsonAPIHandler) GETProfile(w http.ResponseWriter, r *http.Request) {
	_, peerId := path.Split(r.URL.Path)
	var profile pb.Profile
	var err error
	if peerId == "" || strings.ToLower(peerId) == "profile" || peerId == i.node.IpfsNode.Identity.Pretty() {
		profile, err = i.node.GetProfile()
		if err != nil {
			ErrorResponse(w, http.StatusNotFound, err.Error())
			return
		}
	} else {
		if strings.HasPrefix(peerId, "@") {
			peerId, err = i.node.Resolver.Resolve(peerId)
			if err != nil {
				ErrorResponse(w, http.StatusNotFound, err.Error())
				return
			}
		}
		p, err := ipfs.ResolveThenCat(i.node.Context, ipnspath.FromString(path.Join(peerId, "profile")))
		if err != nil {
			ErrorResponse(w, http.StatusNotFound, err.Error())
			return
		}
		err = jsonpb.UnmarshalString(string(p), &profile)
		if err != nil {
			ErrorResponse(w, http.StatusNotFound, err.Error())
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=600, immutable")
	}
	m := jsonpb.Marshaler{
		EnumsAsInts:  false,
		EmitDefaults: true,
		Indent:       "    ",
		OrigName:     false,
	}
	out, err := m.MarshalToString(&profile)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, out)
}

func (i *jsonAPIHandler) GETFollowsMe(w http.ResponseWriter, r *http.Request) {
	_, peerId := path.Split(r.URL.Path)
	fmt.Fprintf(w, `{"followsMe": "%t"}`, i.node.Datastore.Followers().FollowsMe(peerId))
}

func (i *jsonAPIHandler) GETIsFollowing(w http.ResponseWriter, r *http.Request) {
	_, peerId := path.Split(r.URL.Path)
	fmt.Fprintf(w, `{"isFollowing": "%t"}`, i.node.Datastore.Following().IsFollowing(peerId))
}

func (i *jsonAPIHandler) POSTOrderConfirmation(w http.ResponseWriter, r *http.Request) {
	type orderConf struct {
		OrderId string `json:"orderId"`
		Reject  bool   `json:"reject"`
	}
	decoder := json.NewDecoder(r.Body)
	var conf orderConf
	err := decoder.Decode(&conf)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	contract, state, funded, records, _, err := i.node.Datastore.Sales().GetByOrderId(conf.OrderId)
	if err != nil {
		ErrorResponse(w, http.StatusNotFound, err.Error())
		return
	}
	if state != pb.OrderState_PENDING {
		ErrorResponse(w, http.StatusBadRequest, "order has already been confirmed")
		return
	}
	if !funded && !conf.Reject {
		ErrorResponse(w, http.StatusBadRequest, "payment address must be funded before confirmation")
		return
	}
	if !conf.Reject {
		err := i.node.ConfirmOfflineOrder(contract, records)
		if err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		err := i.node.RejectOfflineOrder(contract, records)
		if err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	fmt.Fprint(w, `{}`)
	return
}

func (i *jsonAPIHandler) POSTOrderCancel(w http.ResponseWriter, r *http.Request) {
	type orderCancel struct {
		OrderId string `json:"orderId"`
	}
	decoder := json.NewDecoder(r.Body)
	var can orderCancel
	err := decoder.Decode(&can)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	contract, state, _, records, _, err := i.node.Datastore.Purchases().GetByOrderId(can.OrderId)
	if err != nil {
		ErrorResponse(w, http.StatusNotFound, "order not found")
		return
	}
	if state != pb.OrderState_PENDING {
		ErrorResponse(w, http.StatusBadRequest, "order has already been confirmed")
		return
	}
	err = i.node.CancelOfflineOrder(contract, records)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, `{}`)
	return
}

func (i *jsonAPIHandler) POSTResyncBlockchain(w http.ResponseWriter, r *http.Request) {
	i.node.Wallet.ReSyncBlockchain(0)
	fmt.Fprint(w, `{}`)
	return
}

func (i *jsonAPIHandler) GETOrder(w http.ResponseWriter, r *http.Request) {
	_, orderId := path.Split(r.URL.Path)
	var err error
	var isSale bool
	var contract *pb.RicardianContract
	var state pb.OrderState
	var funded bool
	var records []*spvwallet.TransactionRecord
	var read bool
	contract, state, funded, records, read, err = i.node.Datastore.Purchases().GetByOrderId(orderId)
	if err != nil {
		contract, state, funded, records, read, err = i.node.Datastore.Sales().GetByOrderId(orderId)
		if err != nil {
			ErrorResponse(w, http.StatusNotFound, "Order not found")
			return
		}
		isSale = true
	}
	resp := new(pb.OrderRespApi)
	resp.Contract = contract
	resp.Funded = funded
	resp.Read = read
	resp.State = state

	txs := []*pb.TransactionRecord{}
	for _, r := range records {
		tx := new(pb.TransactionRecord)
		tx.Txid = r.Txid
		tx.Value = r.Value
		// TODO: add confirmations
		txs = append(txs, tx)
	}

	resp.Transactions = txs

	m := jsonpb.Marshaler{
		EnumsAsInts:  false,
		EmitDefaults: true,
		Indent:       "    ",
		OrigName:     false,
	}
	out, err := m.MarshalToString(resp)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	if isSale {
		i.node.Datastore.Sales().MarkAsRead(orderId)
	} else {
		i.node.Datastore.Purchases().MarkAsRead(orderId)
	}
	fmt.Fprint(w, out)
}

func (i *jsonAPIHandler) POSTShutdown(w http.ResponseWriter, r *http.Request) {
	shutdown := func() {
		log.Info("OpenBazaar Server shutting down...")
		time.Sleep(time.Second)
		if core.Node != nil {
			core.Node.Datastore.Close()
			repoLockFile := filepath.Join(core.Node.RepoPath, lockfile.LockFile)
			os.Remove(repoLockFile)
			core.Node.Wallet.Close()
			core.Node.IpfsNode.Close()
		}
		os.Exit(1)
	}
	go shutdown()
	fmt.Fprint(w, `{}`)
	return
}

func (i *jsonAPIHandler) POSTRefund(w http.ResponseWriter, r *http.Request) {
	type orderCancel struct {
		OrderId string `json:"orderId"`
	}
	decoder := json.NewDecoder(r.Body)
	var can orderCancel
	err := decoder.Decode(&can)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	contract, state, _, records, _, err := i.node.Datastore.Sales().GetByOrderId(can.OrderId)
	if err != nil {
		ErrorResponse(w, http.StatusNotFound, "order not found")
		return
	}
	if (state != pb.OrderState_FUNDED) && (state != pb.OrderState_FULFILLED) {
		ErrorResponse(w, http.StatusBadRequest, "order must be funded and not complete or disputed before refunding")
		return
	}
	err = i.node.RefundOrder(contract, records)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, `{}`)
	return
}

func (i *jsonAPIHandler) GETModerators(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("async")
	async, _ := strconv.ParseBool(query)

	ctx := context.Background()
	if !async {
		peerInfoList, err := ipfs.FindPointers(i.node.IpfsNode.Routing.(*routing.IpfsDHT), ctx, core.ModeratorPointerID, 64)
		if err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		var mods []string
		for _, p := range peerInfoList {
			addr := p.Addrs[0]
			if addr.Protocols()[0].Code != multiaddr.P_IPFS {
				continue
			}
			val, err := addr.ValueForProtocol(multiaddr.P_IPFS)
			if err != nil {
				continue
			}
			mh, err := multihash.FromB58String(val)
			if err != nil {
				continue
			}
			d, err := multihash.Decode(mh)
			if err != nil {
				continue
			}
			mods = append(mods, string(d.Digest))
		}
		resp, err := json.MarshalIndent(mods, "", "    ")
		if err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		fmt.Fprint(w, string(resp))
	} else {
		idBytes := make([]byte, 16)
		rand.Read(idBytes)
		id := base58.Encode(idBytes)

		type resp struct {
			Id string `json:"id"`
		}
		response := resp{id}
		respJson, _ := json.MarshalIndent(response, "", "    ")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, string(respJson))
		peerChan := ipfs.FindPointersAsync(i.node.IpfsNode.Routing.(*routing.IpfsDHT), ctx, core.ModeratorPointerID, 64)

		type wsResp struct {
			Id        string `json:"id"`
			Moderator string `json:"moderator"`
		}
		for p := range peerChan {
			addr := p.Addrs[0]
			if addr.Protocols()[0].Code != multiaddr.P_IPFS {
				continue
			}
			val, err := addr.ValueForProtocol(multiaddr.P_IPFS)
			if err != nil {
				continue
			}
			mh, err := multihash.FromB58String(val)
			if err != nil {
				continue
			}
			d, err := multihash.Decode(mh)
			if err != nil {
				continue
			}
			resp := wsResp{id, string(d.Digest)}
			respJson, err := json.MarshalIndent(resp, "", "    ")
			if err != nil {
				continue
			}
			i.node.Broadcast <- respJson
		}
	}
}

func (i *jsonAPIHandler) POSTOrderFulfill(w http.ResponseWriter, r *http.Request) {
	decoder := json.NewDecoder(r.Body)
	var fulfill pb.OrderFulfillment
	err := decoder.Decode(&fulfill)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	contract, state, _, records, _, err := i.node.Datastore.Sales().GetByOrderId(fulfill.OrderId)
	if err != nil {
		ErrorResponse(w, http.StatusNotFound, "order not found")
		return
	}
	if state != pb.OrderState_FUNDED {
		ErrorResponse(w, http.StatusBadRequest, "order must be funded before fulfilling")
		return
	}
	err = i.node.FulfillOrder(&fulfill, contract, records)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, `{}`)
	return
}

func (i *jsonAPIHandler) POSTOrderComplete(w http.ResponseWriter, r *http.Request) {
	checkRatingValue := func(val int) {
		if val < core.RatingMin || val > core.RatingMax {
			ErrorResponse(w, http.StatusBadRequest, "rating values must be between 1 and 5")
			return
		}
	}
	decoder := json.NewDecoder(r.Body)
	var or core.OrderRatings
	err := decoder.Decode(&or)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	contract, state, _, records, _, err := i.node.Datastore.Purchases().GetByOrderId(or.OrderId)
	if err != nil {
		ErrorResponse(w, http.StatusNotFound, "order not found")
		return
	}
	for _, rd := range or.Ratings {
		if rd.Slug == "" {
			ErrorResponse(w, http.StatusBadRequest, "rating must contain the slug")
			return
		}
		checkRatingValue(rd.Overall)
		checkRatingValue(rd.Quality)
		checkRatingValue(rd.Description)
		checkRatingValue(rd.DeliverySpeed)
		checkRatingValue(rd.CustomerService)
		if len(rd.Review) > core.ReviewMaxCharacters {
			ErrorResponse(w, http.StatusBadRequest, "too many characters in review")
			return
		}
	}

	if state != pb.OrderState_FULFILLED && state != pb.OrderState_RESOLVED {
		ErrorResponse(w, http.StatusBadRequest, "order must be either fulfilled or in closed dispute state to leave the rating")
		return
	}

	err = i.node.CompleteOrder(&or, contract, records)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, `{}`)
	return
}

func (i *jsonAPIHandler) POSTOpenDispute(w http.ResponseWriter, r *http.Request) {
	type dispute struct {
		OrderID string `json:"orderId"`
		Claim   string `json:"claim"`
	}
	decoder := json.NewDecoder(r.Body)
	var d dispute
	err := decoder.Decode(&d)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	var isSale bool
	var contract *pb.RicardianContract
	var state pb.OrderState
	var records []*spvwallet.TransactionRecord
	contract, state, _, records, _, err = i.node.Datastore.Purchases().GetByOrderId(d.OrderID)
	if err != nil {
		contract, state, _, records, _, err = i.node.Datastore.Sales().GetByOrderId(d.OrderID)
		if err != nil {
			ErrorResponse(w, http.StatusNotFound, "Order not found")
			return
		}
		isSale = true
	}
	if contract.BuyerOrder.Payment.Method != pb.Order_Payment_MODERATED {
		ErrorResponse(w, http.StatusBadRequest, "Only moderated orders can be disputed")
		return
	}

	if isSale && (state != pb.OrderState_FUNDED && state != pb.OrderState_FULFILLED) {
		ErrorResponse(w, http.StatusBadRequest, "Order must be either funded or fulfilled to start a dispute")
		return
	}
	if !isSale && (state != pb.OrderState_CONFIRMED && state != pb.OrderState_FUNDED && state != pb.OrderState_FULFILLED) {
		ErrorResponse(w, http.StatusBadRequest, "Order must be either confirmed, funded, or fulfilled to start a dispute")
		return
	}

	err = i.node.OpenDispute(d.OrderID, contract, records, d.Claim)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, `{}`)
	return
}

func (i *jsonAPIHandler) POSTCloseDispute(w http.ResponseWriter, r *http.Request) {
	type dispute struct {
		OrderID          string  `json:"orderId"`
		Resolution       string  `json:"resolution"`
		BuyerPercentage  float32 `json:"buyerPercentage"`
		VendorPercentage float32 `json:"vendorPercentage"`
	}
	decoder := json.NewDecoder(r.Body)
	var d dispute
	err := decoder.Decode(&d)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	err = i.node.CloseDispute(d.OrderID, d.BuyerPercentage, d.VendorPercentage, d.Resolution)
	if err != nil && err == core.ErrCaseNotFound {
		ErrorResponse(w, http.StatusNotFound, err.Error())
		return
	} else if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, `{}`)
	return
}

func (i *jsonAPIHandler) GETCase(w http.ResponseWriter, r *http.Request) {
	_, orderId := path.Split(r.URL.Path)
	buyerContract, vendorContract, buyerErrors, vendorErrors, state, read, date, buyerOpened, claim, resolution, err := i.node.Datastore.Cases().GetCaseMetadata(orderId)
	if err != nil {
		ErrorResponse(w, http.StatusNotFound, err.Error())
		return
	}

	resp := new(pb.CaseRespApi)
	ts := new(timestamp.Timestamp)
	ts.Seconds = int64(date.Unix())
	ts.Nanos = 0
	resp.BuyerContract = buyerContract
	resp.VendorContract = vendorContract
	resp.BuyerOpened = buyerOpened
	resp.BuyerContractValidationErrors = buyerErrors
	resp.VendorContractValidationErrors = vendorErrors
	resp.Read = read
	resp.State = state
	resp.Claim = claim
	resp.Resolution = resolution

	m := jsonpb.Marshaler{
		EnumsAsInts:  false,
		EmitDefaults: true,
		Indent:       "    ",
		OrigName:     false,
	}
	out, err := m.MarshalToString(resp)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	i.node.Datastore.Cases().MarkAsRead(orderId)
	fmt.Fprint(w, out)
}

func (i *jsonAPIHandler) POSTReleaseFunds(w http.ResponseWriter, r *http.Request) {
	type release struct {
		OrderID string `json:"orderId"`
	}
	decoder := json.NewDecoder(r.Body)
	var rel release
	err := decoder.Decode(&rel)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	var contract *pb.RicardianContract
	var state pb.OrderState
	var records []*spvwallet.TransactionRecord
	contract, state, _, records, _, err = i.node.Datastore.Purchases().GetByOrderId(rel.OrderID)
	if err != nil {
		contract, state, _, records, _, err = i.node.Datastore.Sales().GetByOrderId(rel.OrderID)
		if err != nil {
			ErrorResponse(w, http.StatusNotFound, "Order not found")
			return
		}
	}

	if state != pb.OrderState_DECIDED {
		ErrorResponse(w, http.StatusBadRequest, "Order must be in DECIDED state to release funds")
		return
	}

	err = i.node.ReleaseFunds(contract, records)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, `{}`)
	return
}

func (i *jsonAPIHandler) POSTChat(w http.ResponseWriter, r *http.Request) {
	decoder := json.NewDecoder(r.Body)
	var chat repo.ChatMessage
	err := decoder.Decode(&chat)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(chat.Subject) > 500 {
		ErrorResponse(w, http.StatusBadRequest, "Subjuct line is too long")
		return
	}
	if len(chat.Message) > 20000 {
		ErrorResponse(w, http.StatusBadRequest, "Subjuct line is too long")
		return
	}

	t := time.Now()
	ts := new(timestamp.Timestamp)
	ts.Seconds = t.Unix()
	var flag pb.Chat_Flag
	if chat.Message == "" {
		flag = pb.Chat_TYPING
	} else {
		flag = pb.Chat_MESSAGE
	}
	chatPb := &pb.Chat{
		Subject:   chat.Subject,
		Message:   chat.Message,
		Timestamp: ts,
		Flag:      flag,
	}
	err = i.node.SendChat(chat.PeerId, nil, chatPb)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Put to database
	if chatPb.Flag == pb.Chat_MESSAGE {
		err = i.node.Datastore.Chat().Put(chat.PeerId, chat.Subject, chat.Message, t, false, true)
		if err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	fmt.Fprint(w, `{}`)
	return
}

func (i *jsonAPIHandler) GETChatMessages(w http.ResponseWriter, r *http.Request) {
	_, peerId := path.Split(r.URL.Path)
	limit := r.URL.Query().Get("limit")
	if limit == "" {
		limit = "-1"
	}
	l, err := strconv.Atoi(limit)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	offset := r.URL.Query().Get("offsetId")
	offsetId := 0
	if offset != "" {
		offsetId, err = strconv.Atoi(offset)
		if err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	messages := i.node.Datastore.Chat().GetMessages(peerId, r.URL.Query().Get("subject"), offsetId, int(l))

	ret, err := json.MarshalIndent(messages, "", "    ")
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	if string(ret) == "null" {
		ret = []byte("[]")
	}
	fmt.Fprint(w, string(ret))
	return
}

func (i *jsonAPIHandler) GETChatConversations(w http.ResponseWriter, r *http.Request) {
	conversations := i.node.Datastore.Chat().GetConversations()
	ret, err := json.MarshalIndent(conversations, "", "    ")
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	if string(ret) == "null" {
		ret = []byte("[]")
	}
	fmt.Fprint(w, string(ret))
	return
}

func (i *jsonAPIHandler) POSTMarkChatAsRead(w http.ResponseWriter, r *http.Request) {
	type peerId struct {
		PeerID string `json:"peerId"`
	}
	decoder := json.NewDecoder(r.Body)
	var p peerId
	err := decoder.Decode(&p)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	err = i.node.Datastore.Chat().MarkAsRead(p.PeerID)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, `{}`)
}

func (i *jsonAPIHandler) DELETEChatMessage(w http.ResponseWriter, r *http.Request) {
	type messagID struct {
		MessageID int `json:"messageId"`
	}
	decoder := json.NewDecoder(r.Body)
	var m messagID
	err := decoder.Decode(&m)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	err = i.node.Datastore.Chat().DeleteMessage(m.MessageID)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, `{}`)
}

func (i *jsonAPIHandler) DELETEChatConversation(w http.ResponseWriter, r *http.Request) {
	type peerId struct {
		PeerID string `json:"peerId"`
	}
	decoder := json.NewDecoder(r.Body)
	var p peerId
	err := decoder.Decode(&p)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	err = i.node.Datastore.Chat().DeleteConversation(p.PeerID)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, `{}`)
}

func (i *jsonAPIHandler) GETNotifications(w http.ResponseWriter, r *http.Request) {
	limit := r.URL.Query().Get("limit")
	if limit == "" {
		limit = "-1"
	}
	l, err := strconv.Atoi(limit)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	offset := r.URL.Query().Get("offsetId")
	offsetId := 0
	if offset != "" {
		offsetId, err = strconv.Atoi(offset)
		if err != nil {
			ErrorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	notifs := i.node.Datastore.Notifications().GetAll(offsetId, int(l))

	ret, err := json.MarshalIndent(notifs, "", "    ")
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	if string(ret) == "null" {
		ret = []byte("[]")
	}
	fmt.Fprint(w, string(ret))
	return
}

func (i *jsonAPIHandler) POSTMarkNotificationAsRead(w http.ResponseWriter, r *http.Request) {
	type id struct {
		ID int `json:"id"`
	}
	decoder := json.NewDecoder(r.Body)
	var p id
	err := decoder.Decode(&p)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	err = i.node.Datastore.Notifications().MarkAsRead(p.ID)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, `{}`)
}

func (i *jsonAPIHandler) DELETENotification(w http.ResponseWriter, r *http.Request) {
	type id struct {
		ID int `json:"id"`
	}
	decoder := json.NewDecoder(r.Body)
	var p id
	err := decoder.Decode(&p)
	if err != nil {
		ErrorResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	err = i.node.Datastore.Notifications().Delete(p.ID)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Fprint(w, `{}`)
}

func (i *jsonAPIHandler) GETImage(w http.ResponseWriter, r *http.Request) {
	_, imageHash := path.Split(r.URL.Path)
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	dr, err := coreunix.Cat(ctx, i.node.IpfsNode, "/ipfs/"+imageHash)
	if err != nil {
		ErrorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer dr.Close()
	w.Header().Set("Cache-Control", "public, max-age=29030400, immutable")
	w.Header().Del("Content-Type")
	http.ServeContent(w, r, imageHash, time.Now(), dr)
}
