package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"

	"github.com/flet-dev/flet/server/config"
	"github.com/flet-dev/flet/server/page"
	page_connection "github.com/flet-dev/flet/server/page/connection"
	"github.com/flet-dev/flet/server/store"
	"github.com/gin-gonic/contrib/secure"
	"github.com/gin-gonic/contrib/static"
	"github.com/gin-gonic/gin"
)

const (
	apiRoutePrefix      string = "/api"
	siteDefaultDocument string = "index.html"
)

var (
	Port int = 8550
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

func Start(ctx context.Context, wg *sync.WaitGroup, serverPort int, contentDir string, assetsDir string) {
	defer wg.Done()

	if contentDir == "" {
		log.Fatalf("contentDir is not set")
	}

	Port = serverPort

	// Set the router as the default one shipped with Gin
	router := gin.Default()

	if config.TrustedProxies() != nil && len(config.TrustedProxies()) > 0 {
		log.Println("Trusted proxies:", config.TrustedProxies())
		router.SetTrustedProxies(config.TrustedProxies())
	}

	// force SSL
	if config.ForceSSL() {
		router.Use(secure.Secure(secure.Options{
			AllowedHosts:          []string{},
			SSLRedirect:           true,
			SSLHost:               "", // use the same host
			SSLProxyHeaders:       map[string]string{"X-Forwarded-Proto": "https"},
			STSSeconds:            315360000,
			STSIncludeSubdomains:  true,
			FrameDeny:             true,
			ContentTypeNosniff:    true,
			BrowserXssFilter:      true,
			ContentSecurityPolicy: "",
		}))
	}

	mime.AddExtensionType(".js", "application/javascript")

	// Serve frontend static files
	assetsFS := newAssetsFS(contentDir, assetsDir)
	router.Use(static.Serve("/", assetsFS))

	// WebSockets
	router.GET("/ws", func(c *gin.Context) {
		websocketHandler(c)
	})

	// Setup route group for the API
	api := router.Group(apiRoutePrefix)
	{
		api.GET("/", func(c *gin.Context) {
			time.Sleep(4 * time.Second)
			c.JSON(http.StatusOK, gin.H{
				"message": "pong",
			})
		})
	}

	api.GET("/oauth/redirect", oauthCallbackHandler)
	api.PUT("/upload", uploadFileAsStream)

	// unknown API routes - 404, all the rest - index.html
	router.NoRoute(func(c *gin.Context) {

		if !strings.HasPrefix(c.Request.RequestURI, apiRoutePrefix+"/") {
			baseHref := strings.Trim(c.Request.URL.Path, "/")
			log.Debugln("Request path:", baseHref)

			if baseHref != "" {
				hrefParts := strings.Split(baseHref, "/")
				if len(hrefParts) > 1 {
					baseHref = strings.Join(hrefParts[:2], "/")
					if store.GetPageByName(baseHref) == nil {
						// fallback to index page
						baseHref = ""
					}
				} else {
					baseHref = ""
				}
			}

			if baseHref != "" {
				baseHref = "/" + baseHref + "/"
			} else {
				baseHref = "/"
			}

			index, _ := assetsFS.Open(siteDefaultDocument)
			indexData, _ := io.ReadAll(index)

			// base path
			indexData = bytes.Replace(indexData,
				[]byte("<base href=\"/\">"),
				[]byte("<base href=\""+baseHref+"\">"), 1)

			// route URL strategy
			indexData = bytes.Replace(indexData,
				[]byte("%FLET_ROUTE_URL_STRATEGY%"),
				[]byte(config.RouteUrlStrategy()), 1)

			// web renderer
			if config.WebRenderer() != "" {
				indexData = bytes.Replace(indexData,
					[]byte("<!-- flutterWebRenderer -->"),
					[]byte(fmt.Sprintf("<script>var flutterWebRenderer=\"%s\";</script>", config.WebRenderer())), 1)
			}

			// color emoji
			indexData = bytes.Replace(indexData,
				[]byte("<!-- useColorEmoji -->"),
				[]byte(fmt.Sprintf("<script>var useColorEmoji=%v;</script>", config.UseColorEmoji())), 1)

			c.Data(http.StatusOK, "text/html", indexData)
		} else {
			// API not found
			c.JSON(http.StatusNotFound, gin.H{
				"message": "API endpoint not found",
			})
		}
	})

	addr := fmt.Sprintf("%s:%d", config.ServerIP(), serverPort)
	log.Println("Starting server on", addr)

	// Start and run the server
	srv := &http.Server{
		Addr:    addr,
		Handler: router,
	}

	// Initializing the server in a goroutine so that
	// it won't block the graceful shutdown handling below
	go func() {
		for i := 1; i < 10; i++ {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				if i == 9 {
					log.Fatalf("listen: %s\n", err)
				}
				time.Sleep(time.Duration(100) * time.Millisecond)
				continue
			}
			break
		}
	}()

	go func() {
		page.RunBackgroundTasks(ctx)
	}()

	<-ctx.Done()

	log.Println("Shutting down server...")

	// The context is used to inform the server it has 5 seconds to finish
	// the request it is currently handling
	ctxShutDown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctxShutDown); err != nil {
		log.Fatal("Server forced to shutdown:", err)
	}

	log.Println("Server exited")
}

func websocketHandler(c *gin.Context) {

	upgrader.CheckOrigin = func(r *http.Request) bool {
		return true
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Errorln("Error upgrading WebSocket connection:", err)
		return
	}

	wsc := page_connection.NewWebSocket(conn)
	page.NewClient(wsc, c.ClientIP(), c.Request.UserAgent())
}
