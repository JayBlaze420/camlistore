/*
Copyright 2011 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package server

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"camlistore.org/pkg/blob"
	"camlistore.org/pkg/blobserver"
	"camlistore.org/pkg/fileembed"
	"camlistore.org/pkg/httputil"
	"camlistore.org/pkg/jsonconfig"
	"camlistore.org/pkg/jsonsign/signhandler"
	"camlistore.org/pkg/misc/closure"
	"camlistore.org/pkg/search"
	uistatic "camlistore.org/server/camlistored/ui"
	closurestatic "camlistore.org/server/camlistored/ui/closure"
)

var (
	staticFilePattern = regexp.MustCompile(`^([a-zA-Z0-9\-\_]+\.(html|js|css|png|jpg|gif))$`)
	identOrDotPattern = regexp.MustCompile(`^[a-zA-Z\_]+(\.[a-zA-Z\_]+)*$`)

	// Download URL suffix:
	//   $1: blobref (checked in download handler)
	//   $2: optional "/filename" to be sent as recommended download name,
	//       if sane looking
	downloadPattern = regexp.MustCompile(`^download/([^/]+)(/.*)?$`)

	thumbnailPattern = regexp.MustCompile(`^thumbnail/([^/]+)(/.*)?$`)
	treePattern      = regexp.MustCompile(`^tree/([^/]+)(/.*)?$`)
	closurePattern   = regexp.MustCompile(`^closure/(([^/]+)(/.*)?)$`)
)

// UIHandler handles serving the UI and discovery JSON.
type UIHandler struct {
	// JSONSignRoot is the optional path or full URL to the JSON
	// Signing helper. Only used by the UI and thus necessary if
	// UI is true.
	// TODO(bradfitz): also move this up to the root handler,
	// if we start having clients (like phones) that we want to upload
	// but don't trust to have private signing keys?
	JSONSignRoot string

	PublishRoots map[string]*PublishHandler

	prefix string // of the UI handler itself
	root   *RootHandler
	sigh   *signhandler.Handler // or nil

	Cache blobserver.Storage // or nil
	sc    ScaledImage        // cache for scaled images, optional

	// sourceRoot optionally specifies the path to root of Camlistore's
	// source. If empty, the UI files must be compiled in to the
	// binary (with go run make.go).  This comes from the "sourceRoot"
	// ui handler config option.
	sourceRoot string

	uiDir string // if sourceRoot != "", this is sourceRoot+"/server/camlistored/ui"

	// closureHandler serves the Closure JS files.
	closureHandler http.Handler
}

func init() {
	blobserver.RegisterHandlerConstructor("ui", uiFromConfig)
}

func uiFromConfig(ld blobserver.Loader, conf jsonconfig.Obj) (h http.Handler, err error) {
	ui := &UIHandler{
		prefix:       ld.MyPrefix(),
		JSONSignRoot: conf.OptionalString("jsonSignRoot", ""),
		sourceRoot:   conf.OptionalString("sourceRoot", ""),
	}
	pubRoots := conf.OptionalList("publishRoots")
	cachePrefix := conf.OptionalString("cache", "")
	scType := conf.OptionalString("scaledImage", "")
	if err = conf.Validate(); err != nil {
		return
	}

	if ui.JSONSignRoot != "" {
		h, _ := ld.GetHandler(ui.JSONSignRoot)
		if sigh, ok := h.(*signhandler.Handler); ok {
			ui.sigh = sigh
		}
	}

	ui.PublishRoots = make(map[string]*PublishHandler)
	for _, pubRoot := range pubRoots {
		h, err := ld.GetHandler(pubRoot)
		if err != nil {
			return nil, fmt.Errorf("UI handler's publishRoots references invalid %q", pubRoot)
		}
		pubh, ok := h.(*PublishHandler)
		if !ok {
			return nil, fmt.Errorf("UI handler's publishRoots references invalid %q; not a PublishHandler", pubRoot)
		}
		ui.PublishRoots[pubRoot] = pubh
	}

	checkType := func(key string, htype string) {
		v := conf.OptionalString(key, "")
		if v == "" {
			return
		}
		ct := ld.GetHandlerType(v)
		if ct == "" {
			err = fmt.Errorf("UI handler's %q references non-existant %q", key, v)
		} else if ct != htype {
			err = fmt.Errorf("UI handler's %q references %q of type %q; expected type %q", key, v, ct, htype)
		}
	}
	checkType("searchRoot", "search")
	checkType("jsonSignRoot", "jsonsign")
	if err != nil {
		return
	}

	if cachePrefix != "" {
		bs, err := ld.GetStorage(cachePrefix)
		if err != nil {
			return nil, fmt.Errorf("UI handler's cache of %q error: %v", cachePrefix, err)
		}
		ui.Cache = bs
		switch scType {
		case "lrucache":
			ui.sc = NewScaledImageLRU()
		default:
			return nil, fmt.Errorf("unsupported ui handler's scType: %q ", scType)
		}
	}

	if ui.sourceRoot == "" {
		ui.sourceRoot = os.Getenv("CAMLI_DEV_CAMLI_ROOT")
		if uistatic.IsAppEngine {
			if _, err = os.Stat(filepath.Join(uistatic.GaeSourceRoot,
				filepath.FromSlash("server/camlistored/ui/index.html"))); err != nil {
				hint := fmt.Sprintf("\"sourceRoot\" was not specified in the config,"+
					" and the default sourceRoot dir %v does not exist or does not contain"+
					" \"server/camlistored/ui/index.html\". dev-appengine can do that for you.",
					uistatic.GaeSourceRoot)
				log.Print(hint)
				return nil, errors.New("No sourceRoot found; UI not available.")
			}
			log.Printf("Using the default \"%v\" as the sourceRoot for AppEngine", uistatic.GaeSourceRoot)
			ui.sourceRoot = uistatic.GaeSourceRoot
		}
	}
	if ui.sourceRoot != "" {
		ui.uiDir = filepath.Join(ui.sourceRoot, filepath.FromSlash("server/camlistored/ui"))
		// Ignore any fileembed files:
		Files = &fileembed.Files{
			DirFallback: filepath.Join(ui.sourceRoot, filepath.FromSlash("pkg/server")),
		}
		uistatic.Files = &fileembed.Files{
			DirFallback: ui.uiDir,
			Listable:    true,
			// In dev_appserver, allow edit-and-reload without
			// restarting. In production, though, it's faster to just
			// slurp it in.
			SlurpToMemory: uistatic.IsProdAppEngine,
		}
	}

	ui.closureHandler, err = ui.makeClosureHandler(ui.sourceRoot)
	if err != nil {
		return nil, fmt.Errorf(`Invalid "sourceRoot" value of %q: %v"`, ui.sourceRoot, err)
	}

	rootPrefix, _, err := ld.FindHandlerByType("root")
	if err != nil {
		return nil, errors.New("No root handler configured, which is necessary for the ui handler")
	}
	if h, err := ld.GetHandler(rootPrefix); err == nil {
		ui.root = h.(*RootHandler)
		ui.root.registerUIHandler(ui)
	} else {
		return nil, errors.New("failed to find the 'root' handler")
	}

	return ui, nil
}

func (ui *UIHandler) makeClosureHandler(root string) (http.Handler, error) {
	return makeClosureHandler(root, "ui")
}

// makeClosureHandler returns a handler to serve Closure files.
// root is either:
// 1) empty: use the Closure files compiled in to the binary (if
//    available), else redirect to the Internet.
// 2) a URL prefix: base of Camlistore to get Closure to redirect to
// 3) a path on disk to the root of camlistore's source (which
//    contains the necessary subset of Closure files)
func makeClosureHandler(root, handlerName string) (http.Handler, error) {
	// devcam server environment variable takes precedence:
	if d := os.Getenv("CAMLI_DEV_CLOSURE_DIR"); d != "" {
		log.Printf("%v: serving Closure from devcam server's $CAMLI_DEV_CLOSURE_DIR: %v", handlerName, d)
		return http.FileServer(http.Dir(d)), nil
	}
	if root == "" {
		fs, err := closurestatic.FileSystem()
		if err == os.ErrNotExist {
			log.Printf("%v: no configured setting or embedded resources; serving Closure via %v", handlerName, closureBaseURL)
			return closureBaseURL, nil
		}
		if err != nil {
			return nil, fmt.Errorf("error loading embedded Closure zip file: %v", err)
		}
		log.Printf("%v: serving Closure from embedded resources", handlerName)
		return http.FileServer(fs), nil
	}
	if strings.HasPrefix(root, "http") {
		log.Printf("%v: serving Closure using redirects to %v", handlerName, root)
		return closureRedirector(root), nil
	}
	fi, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, errors.New("not a directory")
	}
	closureRoot := filepath.Join(root, "third_party", "closure", "lib", "closure")
	_, err = os.Stat(filepath.Join(closureRoot, "goog", "base.js"))
	if err != nil {
		return nil, fmt.Errorf("directory doesn't contain closure/goog/base.js; wrong directory?")
	}
	log.Printf("%v: serving Closure from disk: %v", handlerName, closureRoot)
	return http.FileServer(http.Dir(closureRoot)), nil
}

const closureBaseURL closureRedirector = "https://closure-library.googlecode.com/git"

// closureRedirector is a hack to redirect requests for Closure's million *.js files
// to https://closure-library.googlecode.com/git.
// TODO: this doesn't work when offline. We need to run genjsdeps over all of the Camlistore
// UI to figure out which Closure *.js files to fileembed and generate zembed. Then this
// type can be deleted.
type closureRedirector string

func (base closureRedirector) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	newURL := string(base) + "/" + path.Clean(httputil.PathSuffix(req))
	http.Redirect(rw, req, newURL, http.StatusTemporaryRedirect)
}

func camliMode(req *http.Request) string {
	return req.URL.Query().Get("camli.mode")
}

func wantsDiscovery(req *http.Request) bool {
	return req.Method == "GET" &&
		(req.Header.Get("Accept") == "text/x-camli-configuration" ||
			camliMode(req) == "config")
}

func wantsUploadHelper(req *http.Request) bool {
	return req.Method == "POST" && camliMode(req) == "uploadhelper"
}

func wantsPermanode(req *http.Request) bool {
	return req.Method == "GET" && blob.ValidRefString(req.FormValue("p"))
}

func wantsBlobInfo(req *http.Request) bool {
	return req.Method == "GET" && blob.ValidRefString(req.FormValue("b"))
}

func wantsFileTreePage(req *http.Request) bool {
	return req.Method == "GET" && blob.ValidRefString(req.FormValue("d"))
}

func wantsClosure(req *http.Request) bool {
	if req.Method == "GET" {
		suffix := httputil.PathSuffix(req)
		return closurePattern.MatchString(suffix)
	}
	return false
}

func (ui *UIHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	suffix := httputil.PathSuffix(req)

	rw.Header().Set("Vary", "Accept")
	switch {
	case wantsDiscovery(req):
		ui.root.serveDiscovery(rw, req)
	case wantsUploadHelper(req):
		ui.serveUploadHelper(rw, req)
	case strings.HasPrefix(suffix, "download/"):
		ui.serveDownload(rw, req)
	case strings.HasPrefix(suffix, "thumbnail/"):
		ui.serveThumbnail(rw, req)
	case strings.HasPrefix(suffix, "tree/"):
		ui.serveFileTree(rw, req)
	case wantsClosure(req):
		ui.serveClosure(rw, req)
	default:
		file := ""
		if m := staticFilePattern.FindStringSubmatch(suffix); m != nil {
			file = m[1]
		} else {
			switch {
			case wantsPermanode(req):
				file = "permanode.html"
			case wantsBlobInfo(req):
				file = "blobinfo.html"
			case wantsFileTreePage(req):
				file = "filetree.html"
			case req.URL.Path == httputil.PathBase(req):
				file = "index.html"
			default:
				http.Error(rw, "Illegal URL.", http.StatusNotFound)
				return
			}
		}
		if file == "deps.js" {
			serveDepsJS(rw, req, ui.uiDir)
			return
		}
		serveStaticFile(rw, req, uistatic.Files, file)
	}
}

func serveStaticFile(rw http.ResponseWriter, req *http.Request, root http.FileSystem, file string) {
	f, err := root.Open("/" + file)
	if err != nil {
		http.NotFound(rw, req)
		log.Printf("Failed to open file %q from uistatic.Files: %v", file, err)
		return
	}
	defer f.Close()
	var modTime time.Time
	if fi, err := f.Stat(); err == nil {
		modTime = fi.ModTime()
	}
	http.ServeContent(rw, req, file, modTime, f)
}

func (ui *UIHandler) populateDiscoveryMap(m map[string]interface{}) {
	pubRoots := map[string]interface{}{}
	for key, pubh := range ui.PublishRoots {
		m := map[string]interface{}{
			"name":   pubh.RootName,
			"prefix": []string{key},
			// TODO: include gpg key id
		}
		if sh, ok := ui.root.SearchHandler(); ok {
			pn, err := sh.Index().PermanodeOfSignerAttrValue(sh.Owner(), "camliRoot", pubh.RootName)
			if err == nil {
				m["currentPermanode"] = pn.String()
			}
		}
		pubRoots[pubh.RootName] = m
	}

	uiDisco := map[string]interface{}{
		"jsonSignRoot":    ui.JSONSignRoot,
		"uploadHelper":    ui.prefix + "?camli.mode=uploadhelper", // hack; remove with better javascript
		"downloadHelper":  path.Join(ui.prefix, "download") + "/",
		"directoryHelper": path.Join(ui.prefix, "tree") + "/",
		"publishRoots":    pubRoots,
	}
	if ui.sigh != nil {
		uiDisco["signing"] = ui.sigh.DiscoveryMap(ui.JSONSignRoot)
	}
	for k, v := range uiDisco {
		if _, ok := m[k]; ok {
			log.Fatalf("Duplicate discovery key %q", k)
		}
		m[k] = v
	}
}

func (ui *UIHandler) serveDownload(rw http.ResponseWriter, req *http.Request) {
	if ui.root.Storage == nil {
		http.Error(rw, "No BlobRoot configured", 500)
		return
	}

	suffix := httputil.PathSuffix(req)
	m := downloadPattern.FindStringSubmatch(suffix)
	if m == nil {
		httputil.ErrorRouting(rw, req)
		return
	}

	fbr, ok := blob.Parse(m[1])
	if !ok {
		http.Error(rw, "Invalid blobref", 400)
		return
	}

	dh := &DownloadHandler{
		Fetcher: ui.root.Storage,
		Cache:   ui.Cache,
	}
	dh.ServeHTTP(rw, req, fbr)
}

func (ui *UIHandler) serveThumbnail(rw http.ResponseWriter, req *http.Request) {
	if ui.root.Storage == nil {
		http.Error(rw, "No BlobRoot configured", 500)
		return
	}

	suffix := httputil.PathSuffix(req)
	m := thumbnailPattern.FindStringSubmatch(suffix)
	if m == nil {
		httputil.ErrorRouting(rw, req)
		return
	}

	query := req.URL.Query()
	width, _ := strconv.Atoi(query.Get("mw"))
	height, _ := strconv.Atoi(query.Get("mh"))
	blobref, ok := blob.Parse(m[1])
	if !ok {
		http.Error(rw, "Invalid blobref", 400)
		return
	}

	if width == 0 {
		width = search.MaxImageSize
	}
	if height == 0 {
		height = search.MaxImageSize
	}

	th := &ImageHandler{
		Fetcher:   ui.root.Storage,
		Cache:     ui.Cache,
		MaxWidth:  width,
		MaxHeight: height,
		sc:        ui.sc,
	}
	th.ServeHTTP(rw, req, blobref)
}

func (ui *UIHandler) serveFileTree(rw http.ResponseWriter, req *http.Request) {
	if ui.root.Storage == nil {
		http.Error(rw, "No BlobRoot configured", 500)
		return
	}

	suffix := httputil.PathSuffix(req)
	m := treePattern.FindStringSubmatch(suffix)
	if m == nil {
		httputil.ErrorRouting(rw, req)
		return
	}

	blobref, ok := blob.Parse(m[1])
	if !ok {
		http.Error(rw, "Invalid blobref", 400)
		return
	}

	fth := &FileTreeHandler{
		Fetcher: ui.root.Storage,
		file:    blobref,
	}
	fth.ServeHTTP(rw, req)
}

func (ui *UIHandler) serveClosure(rw http.ResponseWriter, req *http.Request) {
	suffix := httputil.PathSuffix(req)
	if ui.closureHandler == nil {
		log.Printf("%v not served: closure handler is nil", suffix)
		http.NotFound(rw, req)
		return
	}
	m := closurePattern.FindStringSubmatch(suffix)
	if m == nil {
		httputil.ErrorRouting(rw, req)
		return
	}
	req.URL.Path = "/" + m[1]
	ui.closureHandler.ServeHTTP(rw, req)
}

// serveDepsJS serves an auto-generated Closure deps.js file.
func serveDepsJS(rw http.ResponseWriter, req *http.Request, dir string) {
	var root http.FileSystem
	if dir == "" {
		root = uistatic.Files
	} else {
		root = http.Dir(dir)
	}

	b, err := closure.GenDeps(root)
	if err != nil {
		log.Print(err)
		http.Error(rw, "Server error", 500)
		return
	}
	rw.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	rw.Write([]byte("// auto-generated from camlistored\n"))
	rw.Write(b)
}
