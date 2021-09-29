package main

import (
	"context"
	"flag"
	"fmt"
	"image/png"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"time"

	"github.com/boombuler/barcode"
	"github.com/boombuler/barcode/code128"
	"github.com/jlaffaye/ftp"
)

type key int

const (
	requestIDKey key = 0
)

var (
	listenAddr string
	healthy    int32
	directory  string
	ftpClient  ftpStruc
	logger     *log.Logger
)

type ftpStruc struct {
	srvFtp  string
	userFtp string
	pwdFtp  string
}

// ImageTemplate : Template loading Image
var ImageTemplate string = `<!DOCTYPE html>
    <html lang="en"><head></head>
	<body><img src="data:image/jpg;base64,{{.Image}}"></body>`

// ImageNotFound : Template loading error
var ImageNotFound string = `<!DOCTYPE html>
    <html lang="en"><head></head>
	<body><p>Impossible de lire l'attestation vétérinaire. Non Trouvé</p></body>`

// PdfNotFound : Template loading error
var PdfNotFound string = `<!DOCTYPE html>
<html lang="en"><head></head>
<body><p>Impossible de lire l'attestation vétérinaire. Non Trouvé</p></body>`

func main() {
	flag.StringVar(&listenAddr, "listen-addr", ":5000", "server listen address")
	flag.StringVar(&directory, "directory", ".", "directory location document")
	flag.StringVar(&ftpClient.srvFtp, "srvFtp", "localhost", "Ftp servername archive")
	flag.StringVar(&ftpClient.userFtp, "userFtp", "userftp", "Ftp username archive")
	flag.StringVar(&ftpClient.pwdFtp, "pwdFtp", "pwd", "Ftp password archive")
	flag.Parse()

	logger = log.New(os.Stdout, "http: ", log.LstdFlags)
	logger.Println("Server is starting...")

	router := http.NewServeMux()
	router.Handle("/", index())
	router.Handle("/healthz", healthz())
	//router.Handle("/attestation", attestation())
	router.Handle("/attestation", attestationPdf())
	router.Handle("/sampleIdToBarCode", generateBarCode())

	nextRequestID := func() string {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}

	server := &http.Server{
		Addr:         listenAddr,
		Handler:      tracing(nextRequestID)(logging()(router)),
		ErrorLog:     logger,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  15 * time.Second,
	}

	done := make(chan bool)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)

	go func() {
		<-quit
		logger.Println("Server is shutting down...")
		atomic.StoreInt32(&healthy, 0)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		server.SetKeepAlivesEnabled(false)
		if err := server.Shutdown(ctx); err != nil {
			logger.Fatalf("Could not gracefully shutdown the server: %v\n", err)
		}
		close(done)
	}()

	logger.Println("Server is ready to handle requests at", listenAddr)
	atomic.StoreInt32(&healthy, 1)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatalf("Could not listen on %s: %v\n", listenAddr, err)
	}

	<-done
	logger.Println("Server stopped")
}

func index() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "Hello, Folks!")
	})
}

func healthz() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&healthy) == 1 {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "UP")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
}

func generateBarCode() http.Handler {

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		logger.Println("generateBarCode")

		// get search key
		keys, ok := r.URL.Query()["key"]
		if !ok || len(keys[0]) < 1 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		key := keys[0]

		logger.Println("Url Param 'key' is: " + string(key))

		// mapping to png file
		filename := key + ".png"
		currPath := directory + "/" + filename
		logger.Println("Png location: " + currPath)

		// Create the barcode
		bc, _ := code128.Encode(string(key))

		// Scale the barcode to 200x200 pixels
		scaled, _ := barcode.Scale(bc, 200, 200)

		// create the output file
		file, _ := os.Create(currPath)
		defer file.Close()

		// encode the barcode as png
		png.Encode(file, scaled)

		// [TODO] Upload To SRVBDDLOF (directory oracle pour intéger dans le mail)
		fmt.Fprintln(w, "L'étiquette code barre est disponible sous ", currPath)

	})
}

func attestationPdf() http.Handler {

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		logger.Println("attestation")

		// get search key
		keys, ok := r.URL.Query()["key"]
		if !ok || len(keys[0]) < 1 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		key := keys[0]

		logger.Println("Url Param 'key' is: " + string(key))
		logger.Println("directory is: " + directory)

		// mapping to pdf file
		filename := key + ".pdf"
		currPath := directory + "/" + filename
		logger.Println("Pdf location: " + currPath)

		file, err := os.Open(currPath)
		if err != nil {
			logger.Println("unable to find pdf. Trying to search on SRVDATA", err)
			// [TODO] Upload depuis SRVDATA
			_, err := retrieveFromSRVDATA(directory, filename)
			if err != nil {
				logger.Println("unable to find pdf", err)
				w.Write([]byte(PdfNotFound))
				return
			}
		}
		defer file.Close()

		w.Header().Set("Content-Type", "application/pdf; charset=utf-8")
		http.ServeFile(w, r, currPath)
	})
}

func retrieveFromSRVDATA(directory string, filename string) (file *os.File, err error) {

	c, err := ftp.Dial(ftpClient.srvFtp+":21", ftp.DialWithTimeout(5*time.Second))
	if err != nil {
		return file, err
	}

	err = c.Login(ftpClient.userFtp, ftpClient.pwdFtp)
	if err != nil {
		return file, err
	}

	logger.Println("retrieve from SRVDATA : " + filename)
	r, err := c.Retr(filename)
	if err != nil {
		return file, err
	}

	logger.Println("Create temp file: " + directory + "/" + filename)
	dstFile, err := ioutil.TempFile(directory, filename)

	_, err = io.Copy(dstFile, r)
	err = dstFile.Close()

	logger.Println("Rename temp file: " + dstFile.Name() + " to " + directory + "/" + filename)
	os.Rename(dstFile.Name(), directory+"/"+filename)

	if err := c.Quit(); err != nil {
		log.Fatal(err)
	}
	file, err = os.Open(directory + "/" + filename)
	defer file.Close()

	return file, err
}

/*
func attestation() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		log.Println("attestation")

		// get search key
		keys, ok := r.URL.Query()["key"]
		if !ok || len(keys[0]) < 1 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		key := keys[0]

		log.Println("Url Param 'key' is: " + string(key))

		// mapping to image file
		currPath := directory + "/" + key + ".png"
		log.Println("Image location: " + currPath)

		file, err := os.Open(currPath)
		if err != nil {
			log.Println("unable to find image.", err)
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(ImageNotFound))
			return
		}
		defer file.Close()

		// decode file
		imageData, _, err := image.Decode(file)
		if err != nil {
			log.Println("unable to decode imag from file.", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		// encode image to string
		buffer := new(bytes.Buffer)
		if err := png.Encode(buffer, imageData); err != nil {
			log.Println("unable to encode png image.", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		str := base64.StdEncoding.EncodeToString(buffer.Bytes())
		if tmpl, err := template.New("image").Parse(ImageTemplate); err != nil {
			log.Println("unable to parse image template.", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		} else {
			data := map[string]interface{}{"Image": str}
			if err = tmpl.Execute(w, data); err != nil {
				log.Println("unable to execute template.", err)
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}

	})
}
*/

func logging() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				requestID, ok := r.Context().Value(requestIDKey).(string)
				if !ok {
					requestID = "unknown"
				}
				logger.Println(requestID, r.Method, r.URL.Path, r.RemoteAddr, r.UserAgent())
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func tracing(nextRequestID func() string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get("X-Request-Id")
			if requestID == "" {
				requestID = nextRequestID()
			}
			ctx := context.WithValue(r.Context(), requestIDKey, requestID)
			w.Header().Set("X-Request-Id", requestID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
