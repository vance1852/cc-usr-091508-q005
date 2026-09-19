// Package app wires the HTTP API: authentication, role scoping,
// contractor data isolation, and the authorization workflow endpoints.
package app

import (
	"database/sql"
	"net/http"

	"airspace/internal/store"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// Server holds the shared state of the HTTP API.
type Server struct {
	db *sql.DB
}

// NewServer builds the Echo engine with all routes registered.
func NewServer(db *sql.DB) *echo.Echo {
	s := &Server{db: db}
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Use(middleware.Recover())

	e.GET("/healthz", func(c echo.Context) error { return c.JSON(http.StatusOK, map[string]string{"status": "ok"}) })

	api := e.Group("/api", s.auth)

	// 申请与授权
	api.GET("/requests", s.listRequests)
	api.POST("/requests", s.createRequest)
	api.GET("/requests/:id", s.getRequest)
	api.POST("/requests/:id/decisions", s.decide)

	// 救援航线
	api.GET("/routes", s.listRoutes)
	api.POST("/routes", s.createRoute)
	api.POST("/routes/:id", s.updateRoute)

	// 飞行器
	api.GET("/aircraft", s.listAircraft)
	api.POST("/aircraft/:id/link", s.setLink)

	// 凭证
	api.GET("/credentials/mine", s.myCredentials)
	api.POST("/credentials/use", s.useCredential)
	api.POST("/credentials/:id/unfreeze", s.unfreeze)

	// 通知与回执
	api.GET("/notifications/mine", s.myNotifications)
	api.POST("/notifications/:id/ack", s.ackNotification)

	// 航段登记（只增不改）
	api.POST("/segments", s.reportSegment)
	api.GET("/segments/mine", s.mySegments)
	api.GET("/segments", s.listSegments)

	// 指挥概况
	api.GET("/overview", s.overview)

	return e
}

// auth resolves `Authorization: Bearer <token>` to a user row.
func (s *Server) auth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		h := c.Request().Header.Get("Authorization")
		const prefix = "Bearer "
		if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
			return c.JSON(http.StatusUnauthorized, errBody("missing bearer token"))
		}
		u, err := s.userByToken(s.db, h[len(prefix):])
		if err != nil {
			return c.JSON(http.StatusUnauthorized, errBody("invalid token"))
		}
		c.Set("user", u)
		return next(c)
	}
}

func errBody(msg string) map[string]string { return map[string]string{"error": msg} }

func currentUser(c echo.Context) *store.User {
	u, _ := c.Get("user").(*store.User)
	return u
}

func hasRole(u *store.User, roles ...string) bool {
	for _, r := range roles {
		if u.Role == r {
			return true
		}
	}
	return false
}
