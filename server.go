package main

import (
	"fmt"
	"log"
	"net/url"
	"reflect"
	"strings"
	"unsafe"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/valyala/fasthttp"
)

var proxyClient = &fasthttp.Client{}

func main() {
	var config = LoadConfig()
	var app = Setup(config)

	// Start server
	log.Printf("Listen on port %s", config.Port)
	log.Fatal(app.Listen(fmt.Sprintf(":%s", config.Port)))
}

// Setup Setup a fiber app with all of its routes
func Setup(config Config) *fiber.App {
	app := fiber.New()

	// Config object
	app.Use(func(c *fiber.Ctx) error {
		c.Locals("config", config)
		return c.Next()
	})

	// CORS
	app.Use(cors.New())

	// Logger
	app.Use(logger.New())

	// Handler
	if config.RoutePrefix != "" {
		subRoute := app.Group(config.RoutePrefix)
		subRoute.Get("/ping", pingHandler)
		subRoute.All("/*", handleRequestAndRedirect)
	}
	app.Get("/ping", pingHandler)
	app.All("/*", handleRequestAndRedirect)

	return app
}

// Ping handler
func pingHandler(c *fiber.Ctx) error {
	return c.Send([]byte("pong"))
}

// Given a request send it to the appropriate url
func handleRequestAndRedirect(c *fiber.Ctx) error {
	config := c.Locals("config").(Config)

	upstreamReq := fasthttp.AcquireRequest()
	upstreamResp := fasthttp.AcquireResponse()

	defer fasthttp.ReleaseRequest(upstreamReq)
	defer fasthttp.ReleaseResponse(upstreamResp)

	c.Request().CopyTo(upstreamReq)

	// Trim prefix
	reqURI := string(c.Request().RequestURI())
	if config.RoutePrefix != "" && strings.HasPrefix(reqURI, config.RoutePrefix+"/") {
		reqURI = strings.TrimPrefix(reqURI, config.RoutePrefix)
		upstreamReq.SetRequestURI(reqURI)
	}

	// [WORKAROUND] To "easily" manage multiple Google domains, set this hard condition
	var targetOrigin string
	if strings.HasPrefix(reqURI, "/g/collect") {
		targetOrigin = "https://www.google-analytics.com"
	} else {
		targetOrigin = config.GoogleOrigin
	}

	// Overwrite
	url, _ := url.Parse(targetOrigin)
	upstreamReq.SetHost(url.Host)
	upstreamReq.URI().SetScheme(url.Scheme)

	// Prepare request
	prepareRequest(upstreamReq, c)
	log.Printf("GET %s -> making request to %s", c.Params("*"), upstreamReq.URI().FullURI())

	// Start request to dest URL
	if err := proxyClient.Do(upstreamReq, upstreamResp); err != nil {
		return err
	}

	// Post process the response
	if err := postprocessResponse(upstreamResp, c); err != nil {
		return err
	}

	return nil
}

// Prepare request
func prepareRequest(upstreamResp *fasthttp.Request, c *fiber.Ctx) {
	config := c.Locals("config").(Config)

	for _, name := range strings.Split(config.InjectParamsFromReqHeaders, ",") {
		// Convert header fields to request params
		// e.g. INJECT_PARAMS_FROM_REQ_HEADERS=uip,user-agent
		//   will be add this to the URI: ?uip=[VALUE]&user-agent=[VALUE]
		// To rename the key, use [HEADER_NAME]__[NEW_NAME]
		// e.g. INJECT_PARAMS_FROM_REQ_HEADERS=x-email__uip,user-agent__ua
		if name != "" {
			if strings.Contains(name, "__") {
				ss := strings.Split(name, "__")
				val := c.Get(ss[0])
				upstreamResp.URI().QueryArgs().Add(ss[1], val)
				log.Printf("Added %s=%s to query string\n", ss[1], val)
			} else {
				val := c.Get(name)
				upstreamResp.URI().QueryArgs().Add(name, val)
				log.Printf("Added %s=%s to query string\n", name, val)
			}
		}
	}

	for _, name := range strings.Split(config.SkipParamsFromReqHeaders, ",") {
		// Skip params from original request
		if name != "" {
			upstreamResp.URI().QueryArgs().Del(name)
			log.Printf("Removed %s from query string", name)
		}
	}

	// Overwrite IP, UA
	upstreamResp.URI().QueryArgs().Add("uip", c.IP())
	upstreamResp.URI().QueryArgs().Add("ua", c.Get("User-Agent"))
}

// Post process response
func postprocessResponse(upstreamResp *fasthttp.Response, c *fiber.Ctx) error {
	config := c.Locals("config").(Config)

	// Add header
	upstreamResp.Header.Add("x-proxy-by", "gaxy")

	bodyString, err := GetBodyString(upstreamResp)
	if err != nil {
		return err
	}

	var contentType = string(upstreamResp.Header.ContentType())
	if strings.HasPrefix(contentType, "text/javascript") || strings.HasPrefix(contentType, "application/javascript") {
		find := []string{
			"\"+(a?a+\".\":\"\")+\"analytics.google.com",
			"ssl.google-analytics.com",
			"\"+a+\".google-analytics.com",
			"www.google-analytics.com",
			"google-analytics.com",
			"www.googletagmanager.com",
			"googletagmanager.com",
		}
		currentHost := getGaxyHostName(c)

		for _, toReplace := range find {
			bodyString = strings.ReplaceAll(bodyString, toReplace, currentHost+config.RoutePrefix)
		}
	}

	c.Response().SetBodyString(bodyString)
	c.Response().Header.SetContentType(string(upstreamResp.Header.ContentType()))
	c.Response().SetStatusCode(upstreamResp.StatusCode())

	return nil
}

// GetBodyString get body string from fasthttp.Response
func GetBodyString(r *fasthttp.Response) (string, error) {
	var body []byte
	var err error

	contentEncoding := string(r.Header.Peek("Content-Encoding"))
	switch contentEncoding {
	case "gzip":
		body, err = r.BodyGunzip()
	case "br":
		body, err = r.BodyUnbrotli()
	case "deflate":
		body, err = r.BodyInflate()
	default:
		body = r.Body()
	}
	if err != nil {
		return "", err
	}

	bodyString := string(body)
	return bodyString, nil
}

func getGaxyHostName(c *fiber.Ctx) string {
	if host := c.Get("X-Forwarded-Host", ""); host != "" {
		return host
	}

	return getString(c.Request().URI().Host())
}

func getString(b []byte) string {
	sh := (*reflect.SliceHeader)(unsafe.Pointer(&b))
	return *(*string)(unsafe.Pointer(&reflect.StringHeader{Data: sh.Data, Len: sh.Len}))
}
