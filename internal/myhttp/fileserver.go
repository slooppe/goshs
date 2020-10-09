package myhttp

import (
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/patrickhener/goshs/internal/myca"
	"github.com/patrickhener/goshs/internal/myhtml"
	"github.com/patrickhener/goshs/internal/mylog"
)

type directory struct {
	Path    string
	Content []item
}

type item struct {
	URI  string
	Name string
}

// FileServer holds the fileserver information
type FileServer struct {
	Port       int
	Webroot    string
	SSL        bool
	SelfSigned bool
	MyKey      string
	MyCert     string
	BasicAuth  string
}

// router will hook up the webroot with our fileserver
func (fs *FileServer) router() {
	http.Handle("/", fs)
}

// authRouter will hook up the webroot with the fileserver using basic auth
func (fs *FileServer) authRouter() {
	http.HandleFunc("/", fs.basicAuth(fs.ServeHTTP))
}

// basicAuth is a wrapper to handle the basic auth
func (fs *FileServer) basicAuth(handler http.HandlerFunc) func(w http.ResponseWriter, req *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)

		username, password, authOK := req.BasicAuth()
		if authOK == false {
			http.Error(w, "Not authorized", http.StatusUnauthorized)
			return
		}

		if username != "gopher" || password != fs.BasicAuth {
			http.Error(w, "Not authorized", http.StatusUnauthorized)
			return
		}

		fs.ServeHTTP(w, req)
	}
}

// Start will start the file server
func (fs *FileServer) Start() {
	// init router with or without auth
	if fs.BasicAuth != "" {
		if !fs.SSL {
			log.Printf("WARNING!: You are using basic auth without SSL. Your credentials will be transfered in cleartext. Consider using -s, too.\n")
		}
		log.Printf("Using 'gopher:%+v' as basic auth\n", fs.BasicAuth)
		fs.authRouter()
	} else {
		fs.router()
	}

	// construct server
	add := fmt.Sprintf(":%+v", fs.Port)
	server := http.Server{Addr: add}

	// Check if ssl
	if fs.SSL {
		// Check if selfsigned
		if fs.SelfSigned {
			serverTLSConf, fingerprint256, fingerprint1, err := myca.Setup()
			if err != nil {
				log.Fatalf("Unable to start SSL enabled server: %+v\n", err)
			}
			server.TLSConfig = serverTLSConf
			log.Printf("Serving HTTP on 0.0.0.0 port %+v from %+v with ssl enabled and self-signed certificate\n", fs.Port, fs.Webroot)
			log.Println("WARNING! Be sure to check the fingerprint of certificate")
			log.Printf("SHA-256 Fingerprint: %+v\n", fingerprint256)
			log.Printf("SHA-1   Fingerprint: %+v\n", fingerprint1)
			log.Panic(server.ListenAndServeTLS("", ""))
		} else {
			if fs.MyCert == "" || fs.MyKey == "" {
				log.Fatalln("You need to provide server.key and server.crt if -s and not -ss")
			}

			fingerprint256, fingerprint1, err := myca.ParseAndSum(fs.MyCert)
			if err != nil {
				log.Fatalf("Unable to start SSL enabled server: %+v\n", err)
			}

			log.Printf("Serving HTTP on 0.0.0.0 port %+v from %+v with ssl enabled server key: %+v, server cert: %+v\n", fs.Port, fs.Webroot, fs.MyKey, fs.MyCert)
			log.Println("INFO! You provided a certificate and might want to check the fingerprint nonetheless")
			log.Printf("SHA-256 Fingerprint: %+v\n", fingerprint256)
			log.Printf("SHA-1   Fingerprint: %+v\n", fingerprint1)

			log.Panic(server.ListenAndServeTLS(fs.MyCert, fs.MyKey))
		}
	} else {
		log.Printf("Serving HTTP on 0.0.0.0 port %+v from %+v\n", fs.Port, fs.Webroot)
		log.Panic(server.ListenAndServe())
	}
}

// ServeHTTP will serve the response by leveraging our handler
func (fs *FileServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	defer func() {
		if err := recover(); err != nil {
			http.Error(w, fmt.Sprintf("%+v", err), http.StatusInternalServerError)
		}
	}()

	switch req.Method {
	case "GET":
		fs.handler(w, req)
	case "POST":
		fs.upload(w, req)
	}
}

// handler is the function which actually handles dir or file retrieval
func (fs *FileServer) handler(w http.ResponseWriter, req *http.Request) {
	// Get url so you can extract Headline and title
	upath := req.URL.Path

	// Ignore default browser call to /favicon.ico
	if upath == "/favicon.ico" {
		return
	}

	// Define absolute path
	open := fs.Webroot + path.Clean(upath)

	// Check if you are in a dir
	file, err := os.Open(open)
	if os.IsNotExist(err) {
		// Handle as 404
		fs.handle404(w, req)
		return
	}
	if os.IsPermission(err) {
		// Handle as 500
		fs.handle500(w, req)
		return
	}
	if err != nil {
		// Handle general error
		log.Println(err)
		return
	}
	defer file.Close()

	// Log request
	mylog.LogRequest(req.RemoteAddr, req.Method, req.URL.Path, req.Proto, "200")

	// Switch and check if dir
	stat, _ := file.Stat()
	if stat.IsDir() {
		fs.processDir(w, req, file, upath)
	} else {
		fs.sendFile(w, file)
	}
}

// upload handles the POST request to upload files
func (fs *FileServer) upload(w http.ResponseWriter, req *http.Request) {
	req.ParseMultipartForm(10 << 20)

	file, handler, err := req.FormFile("file")
	if err != nil {
		log.Printf("Error retrieving the file: %+v\n", err)
	}
	defer file.Close()

	// Get url so you can extract Headline and title
	upath := req.URL.Path

	// construct target path
	targetpath := strings.Split(upath, "/")
	targetpath = targetpath[:len(targetpath)-1]
	target := strings.Join(targetpath, "/")

	// Construct absolute savepath
	savepath := fmt.Sprintf("%s%s/%s", fs.Webroot, target, handler.Filename)

	// Create file to write to
	if _, err := os.Create(savepath); err != nil {
		log.Println("ERROR:   Not able to create file on disk")
		fs.handle500(w, req)
	}

	// Read file from post body
	fileBytes, err := ioutil.ReadAll(file)
	if err != nil {
		log.Println("ERROR:   Not able to read file from request")
		fs.handle500(w, req)
	}

	// Write file to disk
	if err := ioutil.WriteFile(savepath, fileBytes, os.ModePerm); err != nil {
		log.Println("ERROR:   Not able to write file to disk")
		fs.handle500(w, req)
	}

	// Log request
	mylog.LogRequest(req.RemoteAddr, req.Method, req.URL.Path, req.Proto, "200")

	// Redirect back from where we came from
	http.Redirect(w, req, target, http.StatusSeeOther)
}

func (fs *FileServer) processDir(w http.ResponseWriter, req *http.Request, file *os.File, relpath string) {
	// Read directory FileInfo
	fis, err := file.Readdir(-1)
	if err != nil {
		fs.handle404(w, req)
		return
	}

	// Create empty slice
	items := make([]item, 0, len(fis))
	// Iterate over FileInfo of dir
	for _, fi := range fis {
		// Set name and uri
		itemname := fi.Name()
		itemuri := url.PathEscape(path.Join(relpath, itemname))
		// Add / to name if dir
		if fi.IsDir() {
			itemname += "/"
		}
		// define item struct
		item := item{
			Name: itemname,
			URI:  itemuri,
		}
		// Add to items slice
		items = append(items, item)
	}

	// Sort slice all lowercase
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})

	// Template parsing and writing to browser
	t := template.New("index")
	t.Parse(myhtml.GetTemplate("display"))
	d := &directory{Path: relpath, Content: items}
	t.Execute(w, d)
}

func (fs *FileServer) sendFile(w http.ResponseWriter, file *os.File) {
	// Write to browser
	io.Copy(w, file)
}

func (fs *FileServer) handle404(w http.ResponseWriter, req *http.Request) {
	mylog.LogRequest(req.RemoteAddr, req.Method, req.URL.Path, req.Proto, "404")
	mylog.LogMessage("404:   File not found")
	t := template.New("404")
	t.Parse(myhtml.GetTemplate("404"))
	t.Execute(w, nil)
}

func (fs *FileServer) handle500(w http.ResponseWriter, req *http.Request) {
	mylog.LogRequest(req.RemoteAddr, req.Method, req.URL.Path, req.Proto, "500")
	mylog.LogMessage("500:   No permission to access the file")
	t := template.New("500")
	t.Parse(myhtml.GetTemplate("500"))
	t.Execute(w, nil)
}
