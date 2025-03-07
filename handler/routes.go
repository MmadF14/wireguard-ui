﻿package handler

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/sessions"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
	"github.com/labstack/gommon/log"
	"github.com/rs/xid"
	"github.com/skip2/go-qrcode"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/MmadF14/wireguard-ui/emailer"
	"github.com/MmadF14/wireguard-ui/model"
	"github.com/MmadF14/wireguard-ui/store"
	"github.com/MmadF14/wireguard-ui/telegram"
	"github.com/MmadF14/wireguard-ui/util"
)

var usernameRegexp = regexp.MustCompile("^\\w[\\w\\-.]*$")

// Health check handler
func Health() echo.HandlerFunc {
	return func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	}
}

func Favicon() echo.HandlerFunc {
	return func(c echo.Context) error {
		if favicon, ok := os.LookupEnv(util.FaviconFilePathEnvVar); ok {
			return c.File(favicon)
		}
		return c.Redirect(http.StatusFound, util.BasePath+"/static/custom/img/favicon.ico")
	}
}

// LoginPage handler
func LoginPage() echo.HandlerFunc {
	return func(c echo.Context) error {
		return c.Render(http.StatusOK, "login.html", map[string]interface{}{})
	}
}

// Login for signing in handler
func Login(db store.IStore) echo.HandlerFunc {
	return func(c echo.Context) error {
		data := make(map[string]interface{})
		err := json.NewDecoder(c.Request().Body).Decode(&data)

		if err != nil {
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Bad post data"})
		}

		username := data["username"].(string)
		password := data["password"].(string)
		rememberMe := data["rememberMe"].(bool)

		if !usernameRegexp.MatchString(username) {
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Please provide a valid username"})
		}

		dbuser, err := db.GetUserByName(username)
		if err != nil {
			log.Infof("Cannot query user %s from DB", username)
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Invalid credentials"})
		}

		userCorrect := subtle.ConstantTimeCompare([]byte(username), []byte(dbuser.Username)) == 1

		var passwordCorrect bool
		if dbuser.PasswordHash != "" {
			match, err := util.VerifyHash(dbuser.PasswordHash, password)
			if err != nil {
				return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot verify password"})
			}
			passwordCorrect = match
		} else {
			passwordCorrect = subtle.ConstantTimeCompare([]byte(password), []byte(dbuser.Password)) == 1
		}

		if userCorrect && passwordCorrect {
			ageMax := 0
			if rememberMe {
				ageMax = 86400 * 7
			}

			cookiePath := util.GetCookiePath()

			sess, _ := session.Get("session", c)
			sess.Options = &sessions.Options{
				Path:     cookiePath,
				MaxAge:   ageMax,
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			}

			// set session_token
			tokenUID := xid.New().String()
			now := time.Now().UTC().Unix()
			sess.Values["username"] = dbuser.Username
			sess.Values["user_hash"] = util.GetDBUserCRC32(dbuser)
			sess.Values["admin"] = dbuser.Admin
			sess.Values["session_token"] = tokenUID
			sess.Values["max_age"] = ageMax
			sess.Values["created_at"] = now
			sess.Values["updated_at"] = now
			sess.Save(c.Request(), c.Response())

			// set session_token in cookie
			cookie := new(http.Cookie)
			cookie.Name = "session_token"
			cookie.Path = cookiePath
			cookie.Value = tokenUID
			cookie.MaxAge = ageMax
			cookie.HttpOnly = true
			cookie.SameSite = http.SameSiteLaxMode
			c.SetCookie(cookie)

			return c.JSON(http.StatusOK, jsonHTTPResponse{true, "Logged in successfully"})
		}

		return c.JSON(http.StatusUnauthorized, jsonHTTPResponse{false, "Invalid credentials"})
	}
}

// GetUsers handler return a JSON list of all users
func GetUsers(db store.IStore) echo.HandlerFunc {
	return func(c echo.Context) error {
		usersList, err := db.GetUsers()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{
				false, fmt.Sprintf("Cannot get user list: %v", err),
			})
		}

		return c.JSON(http.StatusOK, usersList)
	}
}

// GetUser handler returns a JSON object of single user
func GetUser(db store.IStore) echo.HandlerFunc {
	return func(c echo.Context) error {
		username := c.Param("username")

		if !usernameRegexp.MatchString(username) {
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Please provide a valid username"})
		}

		if !isAdmin(c) && (username != currentUser(c)) {
			return c.JSON(http.StatusForbidden, jsonHTTPResponse{false, "Manager cannot access other user data"})
		}

		userData, err := db.GetUserByName(username)
		if err != nil {
			return c.JSON(http.StatusNotFound, jsonHTTPResponse{false, "User not found"})
		}

		return c.JSON(http.StatusOK, userData)
	}
}

// Logout to log a user out
func Logout() echo.HandlerFunc {
	return func(c echo.Context) error {
		clearSession(c)
		return c.Redirect(http.StatusTemporaryRedirect, util.BasePath+"/login")
	}
}

// LoadProfile to load user information
func LoadProfile() echo.HandlerFunc {
	return func(c echo.Context) error {
		return c.Render(http.StatusOK, "profile.html", map[string]interface{}{
			"baseData": model.BaseData{Active: "profile", CurrentUser: currentUser(c), Admin: isAdmin(c)},
		})
	}
}

// UsersSettings handler
func UsersSettings() echo.HandlerFunc {
	return func(c echo.Context) error {
		return c.Render(http.StatusOK, "users_settings.html", map[string]interface{}{
			"baseData": model.BaseData{Active: "users-settings", CurrentUser: currentUser(c), Admin: isAdmin(c)},
		})
	}
}

// UpdateUser to update user information
func UpdateUser(db store.IStore) echo.HandlerFunc {
	return func(c echo.Context) error {
		data := make(map[string]interface{})
		err := json.NewDecoder(c.Request().Body).Decode(&data)

		if err != nil {
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Bad post data"})
		}

		username := data["username"].(string)
		password := data["password"].(string)
		previousUsername := data["previous_username"].(string)
		admin := data["admin"].(bool)

		if !isAdmin(c) && (previousUsername != currentUser(c)) {
			return c.JSON(http.StatusForbidden, jsonHTTPResponse{false, "Manager cannot access other user data"})
		}

		if !isAdmin(c) {
			admin = false
		}

		if !usernameRegexp.MatchString(previousUsername) {
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Please provide a valid username"})
		}

		user, err := db.GetUserByName(previousUsername)
		if err != nil {
			return c.JSON(http.StatusNotFound, jsonHTTPResponse{false, err.Error()})
		}

		if username == "" || !usernameRegexp.MatchString(username) {
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Please provide a valid username"})
		} else {
			user.Username = username
		}

		if username != previousUsername {
			_, err := db.GetUserByName(username)
			if err == nil {
				return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "This username is taken"})
			}
		}

		if password != "" {
			hash, err := util.HashPassword(password)
			if err != nil {
				return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, err.Error()})
			}
			user.PasswordHash = hash
		}

		if previousUsername != currentUser(c) {
			user.Admin = admin
		}

		if err := db.DeleteUser(previousUsername); err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, err.Error()})
		}
		if err := db.SaveUser(user); err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, err.Error()})
		}
		log.Infof("Updated user information successfully")

		if previousUsername == currentUser(c) {
			setUser(c, user.Username, user.Admin, util.GetDBUserCRC32(user))
		}

		return c.JSON(http.StatusOK, jsonHTTPResponse{true, "Updated user information successfully"})
	}
}

// CreateUser to create new user
func CreateUser(db store.IStore) echo.HandlerFunc {
	return func(c echo.Context) error {
		data := make(map[string]interface{})
		err := json.NewDecoder(c.Request().Body).Decode(&data)

		if err != nil {
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Bad post data"})
		}

		var user model.User
		username := data["username"].(string)
		password := data["password"].(string)
		admin := data["admin"].(bool)

		if username == "" || !usernameRegexp.MatchString(username) {
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Please provide a valid username"})
		} else {
			user.Username = username
		}

		{
			_, err := db.GetUserByName(username)
			if err == nil {
				return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "This username is taken"})
			}
		}

		hash, err := util.HashPassword(password)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, err.Error()})
		}
		user.PasswordHash = hash

		user.Admin = admin

		if err := db.SaveUser(user); err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, err.Error()})
		}
		log.Infof("Created user successfully")

		return c.JSON(http.StatusOK, jsonHTTPResponse{true, "Created user successfully"})
	}
}

// RemoveUser handler
func RemoveUser(db store.IStore) echo.HandlerFunc {
	return func(c echo.Context) error {
		data := make(map[string]interface{})
		err := json.NewDecoder(c.Request().Body).Decode(&data)

		if err != nil {
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Bad post data"})
		}

		username := data["username"].(string)

		if !usernameRegexp.MatchString(username) {
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Please provide a valid username"})
		}

		if username == currentUser(c) {
			return c.JSON(http.StatusForbidden, jsonHTTPResponse{false, "User cannot delete itself"})
		}
		// delete user from database

		if err := db.DeleteUser(username); err != nil {
			log.Error("Cannot delete user: ", err)
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot delete user from database"})
		}

		log.Infof("Removed user: %s", username)

		return c.JSON(http.StatusOK, jsonHTTPResponse{true, "User removed"})
	}
}

// WireGuardClients handler
func WireGuardClients(db store.IStore) echo.HandlerFunc {
	return func(c echo.Context) error {
		clientDataList, err := db.GetClients(true)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{
				false, fmt.Sprintf("Cannot get client list: %v", err),
			})
		}

		return c.Render(http.StatusOK, "clients.html", map[string]interface{}{
			"baseData":       model.BaseData{Active: "", CurrentUser: currentUser(c), Admin: isAdmin(c)},
			"clientDataList": clientDataList,
		})
	}
}

// GetClients handler return a JSON list of Wireguard client data
func GetClients(db store.IStore) echo.HandlerFunc {
	return func(c echo.Context) error {
		clientDataList, err := db.GetClients(true)
		if err != nil {
			log.Error("Error getting clients: ", err)
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{
				false, fmt.Sprintf("Cannot get client list: %v", err),
			})
		}

		// اگر لیست خالی باشد، یک آرایه خالی برگردانیم نه null
		if clientDataList == nil {
			clientDataList = make([]model.ClientData, 0)
		}

		// Process each client and fill subnet range
		processedList := make([]model.ClientData, 0, len(clientDataList))
		for _, clientData := range clientDataList {
			if clientData.Client != nil { // اطمینان از معتبر بودن داده
				processedList = append(processedList, util.FillClientSubnetRange(clientData))
			}
		}

		// Return as a structured response
		return c.JSON(http.StatusOK, map[string]interface{}{
			"success": true,
			"data":    processedList,
		})
	}
}

// GetClient handler returns a JSON object of Wireguard client data
func GetClient(db store.IStore) echo.HandlerFunc {
	return func(c echo.Context) error {
		clientID := c.Param("id")

		if _, err := xid.FromString(clientID); err != nil {
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Please provide a valid client ID"})
		}

		qrCodeSettings := model.QRCodeSettings{
			Enabled:    true,
			IncludeDNS: true,
			IncludeMTU: true,
		}

		clientData, err := db.GetClientByID(clientID, qrCodeSettings)
		if err != nil {
			return c.JSON(http.StatusNotFound, jsonHTTPResponse{false, "Client not found"})
		}

		return c.JSON(http.StatusOK, util.FillClientSubnetRange(clientData))
	}
}

// NewClient handler
func NewClient(db store.IStore) echo.HandlerFunc {
	return func(c echo.Context) error {
		data := make(map[string]interface{})
		err := json.NewDecoder(c.Request().Body).Decode(&data)
		if err != nil {
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Bad post data"})
		}

		var client model.Client
		username := data["username"].(string)
		password := data["password"].(string)
		admin := data["admin"].(bool)

		if username == "" || !usernameRegexp.MatchString(username) {
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Please provide a valid username"})
		} else {
			client.Username = username
		}

		{
			_, err := db.GetUserByName(username)
			if err == nil {
				return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "This username is taken"})
			}
		}

		hash, err := util.HashPassword(password)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, err.Error()})
		}
		client.PasswordHash = hash

		client.Admin = admin

		if err := db.SaveClient(client); err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, err.Error()})
		}
		log.Infof("Created user successfully")

		return c.JSON(http.StatusOK, jsonHTTPResponse{true, "Created user successfully"})
	}
}

// EmailClient handler to send the configuration via email
func EmailClient(db store.IStore, mailer emailer.Emailer, emailSubject, emailContent string) echo.HandlerFunc {
	type clientIdEmailPayload struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}

	return func(c echo.Context) error {
		var payload clientIdEmailPayload
		c.Bind(&payload)
		// TODO validate email

		if _, err := xid.FromString(payload.ID); err != nil {
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Please provide a valid client ID"})
		}

		qrCodeSettings := model.QRCodeSettings{
			Enabled:    true,
			IncludeDNS: true,
			IncludeMTU: true,
		}
		clientData, err := db.GetClientByID(payload.ID, qrCodeSettings)
		if err != nil {
			log.Errorf("Cannot generate client id %s config file for downloading: %v", payload.ID, err)
			return c.JSON(http.StatusNotFound, jsonHTTPResponse{false, "Client not found"})
		}

		// build config
		server, _ := db.GetServer()
		globalSettings, _ := db.GetGlobalSettings()
		config := util.BuildClientConfig(*clientData.Client, server, globalSettings)

		cfgAtt := emailer.Attachment{Name: "wg0.conf", Data: []byte(config)}
		var attachments []emailer.Attachment
		if clientData.Client.PrivateKey != "" {
			qrdata, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(clientData.QRCode, "data:image/png;base64,"))
			if err != nil {
				return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "decoding: " + err.Error()})
			}
			qrAtt := emailer.Attachment{Name: "wg.png", Data: qrdata}
			attachments = []emailer.Attachment{cfgAtt, qrAtt}
		} else {
			attachments = []emailer.Attachment{cfgAtt}
		}
		err = mailer.Send(
			clientData.Client.Name,
			payload.Email,
			emailSubject,
			emailContent,
			attachments,
		)

		if err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, err.Error()})
		}

		return c.JSON(http.StatusOK, jsonHTTPResponse{true, "Email sent successfully"})
	}
}

// SendTelegramClient handler to send the configuration via Telegram
func SendTelegramClient(db store.IStore) echo.HandlerFunc {
	type clientIdUseridPayload struct {
		ID     string `json:"id"`
		Userid string `json:"userid"`
	}
	return func(c echo.Context) error {
		var payload clientIdUseridPayload
		c.Bind(&payload)

		clientData, err := db.GetClientByID(payload.ID, model.QRCodeSettings{Enabled: false})
		if err != nil {
			log.Errorf("Cannot generate client id %s config file for downloading: %v", payload.ID, err)
			return c.JSON(http.StatusNotFound, jsonHTTPResponse{false, "Client not found"})
		}

		// build config
		server, _ := db.GetServer()
		globalSettings, _ := db.GetGlobalSettings()
		config := util.BuildClientConfig(*clientData.Client, server, globalSettings)
		configData := []byte(config)
		var qrData []byte

		if clientData.Client.PrivateKey != "" {
			qrData, err = qrcode.Encode(config, qrcode.Medium, 512)
			if err != nil {
				return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "qr gen: " + err.Error()})
			}
		}

		userid, err := strconv.ParseInt(clientData.Client.TgUserid, 10, 64)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "userid: " + err.Error()})
		}

		err = telegram.SendConfig(userid, clientData.Client.Name, configData, qrData, false)

		if err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, err.Error()})
		}

		return c.JSON(http.StatusOK, jsonHTTPResponse{true, "Telegram message sent successfully"})
	}
}

// UpdateClient handler
func UpdateClient(db store.IStore) echo.HandlerFunc {
	return func(c echo.Context) error {
		data := make(map[string]interface{})
		err := json.NewDecoder(c.Request().Body).Decode(&data)
		if err != nil {
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Bad post data"})
		}

		// ... existing validation code ...

		if err := db.SaveClient(*client); err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, err.Error()})
		}

		// کانفیگ به صورت خودکار اعمال نمی‌شود
		return c.JSON(http.StatusOK, jsonHTTPResponse{true, "Updated client successfully"})
	}
}

// SetClientStatus handler to enable / disable a client
func SetClientStatus(db store.IStore) echo.HandlerFunc {
	return func(c echo.Context) error {
		var clientID string
		var status bool
		var isAutomatic bool

		// پشتیبانی از هر دو متد GET و POST
		switch c.Request().Method {
		case "GET":
			// برای GET، پارامترها رو از URL می‌خونیم
			clientID = c.Param("id")
			statusStr := c.Param("status")
			status = statusStr == "true"
			// پارامتر isAutomatic رو از query string می‌خونیم
			automaticStr := c.QueryParam("automatic")
			isAutomatic = automaticStr == "true"
		case "POST":
			// برای POST، اول سعی می‌کنیم از URL بخونیم
			clientID = c.Param("id")
			if clientID == "" {
				// اگر در URL نبود، از body می‌خونیم
				data := make(map[string]interface{})
				if err := json.NewDecoder(c.Request().Body).Decode(&data); err != nil {
					return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Invalid request data"})
				}
				if id, ok := data["id"].(string); ok {
					clientID = id
				}
				if s, ok := data["status"].(bool); ok {
					status = s
				}
				if a, ok := data["automatic"].(bool); ok {
					isAutomatic = a
				}
			} else {
				// اگر در URL بود، از اونجا می‌خونیم
				statusStr := c.Param("status")
				status = statusStr == "true"
				// پارامتر isAutomatic رو از query string می‌خونیم
				automaticStr := c.QueryParam("automatic")
				isAutomatic = automaticStr == "true"
			}
		default:
			return c.JSON(http.StatusMethodNotAllowed, jsonHTTPResponse{false, "Method not allowed"})
		}

		if clientID == "" {
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Client ID is required"})
		}

		if _, err := xid.FromString(clientID); err != nil {
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Please provide a valid client ID"})
		}

		// Get client data
		clientData, err := db.GetClientByID(clientID, model.QRCodeSettings{Enabled: false})
		if err != nil {
			log.Printf("Error getting client: %v", err)
			return c.JSON(http.StatusNotFound, jsonHTTPResponse{false, "Client not found"})
		}

		client := *clientData.Client

		// اگر وضعیت فعلی با وضعیت درخواستی یکسان است، نیازی به تغییر نیست
		if client.Enabled == status {
			return c.JSON(http.StatusOK, jsonHTTPResponse{true, "Client status already set"})
		}

		// اگر می‌خواهیم کلاینت را فعال کنیم، چک می‌کنیم که تاریخ انقضا و حجم مصرفی درست باشد
		if status {
			// بررسی تاریخ انقضا
			if !client.Expiration.IsZero() && time.Now().After(client.Expiration) {
				return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Cannot enable client: expiration date has passed"})
			}

			// بررسی حجم مصرفی
			if client.Quota > 0 && client.UsedQuota >= client.Quota {
				return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Cannot enable client: quota limit exceeded"})
			}
		}

		// Get settings for interface name
		settings, err := db.GetGlobalSettings()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot get global settings"})
		}

		// Get interface name
		interfaceName := "wg0"
		if settings.ConfigFilePath != "" {
			parts := strings.Split(settings.ConfigFilePath, "/")
			if len(parts) > 0 {
				baseName := parts[len(parts)-1]
				interfaceName = strings.TrimSuffix(baseName, ".conf")
			}
		}

		// به‌روزرسانی وضعیت در دیتابیس
		client.Enabled = status
		if err := db.SaveClient(client); err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, err.Error()})
		}
		log.Printf("Changed client %s enabled status to %v", client.ID, status)

		// فقط در صورت غیرفعال‌سازی خودکار، کانفیگ را اعمال کن
		if !status && isAutomatic {
			// Create WireGuard client for direct interface manipulation
			wgClient, err := wgctrl.New()
			if err != nil {
				log.Printf("Error creating WireGuard client: %v", err)
				return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot create WireGuard client"})
			}
			defer wgClient.Close()

			// غیرفعال کردن کلاینت با استفاده از wgctrl
			key, err := wgtypes.ParseKey(client.PublicKey)
			if err != nil {
				return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Invalid public key"})
			}

			peerConfig := wgtypes.PeerConfig{
				PublicKey: key,
				Remove:    true,
			}

			err = wgClient.ConfigureDevice(interfaceName, wgtypes.Config{
				Peers: []wgtypes.PeerConfig{peerConfig},
			})
			if err != nil {
				log.Printf("Error removing peer %s: %v", client.Name, err)
				return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, fmt.Sprintf("Error disabling client: %v", err)})
			}
			log.Printf("Successfully removed peer %s", client.Name)

			// به‌روزرسانی فایل کانفیگ
			server, err := db.GetServer()
			if err != nil {
				return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot get server config"})
			}

			clients, err := db.GetClients(false)
			if err != nil {
				return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot get clients"})
			}

			users, err := db.GetUsers()
			if err != nil {
				return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot get users"})
			}

			if err := util.WriteWireGuardServerConfig(nil, server, clients, users, settings); err != nil {
				log.Printf("Warning: Could not write config file: %v", err)
			}
		}

		return c.JSON(http.StatusOK, jsonHTTPResponse{true, "Changed client status successfully"})
	}
}

// DownloadClient handler
func DownloadClient(db store.IStore) echo.HandlerFunc {
	return func(c echo.Context) error {
		clientID := c.QueryParam("clientid")
		if clientID == "" {
			return c.JSON(http.StatusNotFound, jsonHTTPResponse{false, "Missing clientid parameter"})
		}

		if _, err := xid.FromString(clientID); err != nil {
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Please provide a valid client ID"})
		}

		clientData, err := db.GetClientByID(clientID, model.QRCodeSettings{Enabled: false})
		if err != nil {
			log.Errorf("Cannot generate client id %s config file for downloading: %v", clientID, err)
			return c.JSON(http.StatusNotFound, jsonHTTPResponse{false, "Client not found"})
		}

		// build config
		server, err := db.GetServer()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, err.Error()})
		}
		globalSettings, err := db.GetGlobalSettings()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, err.Error()})
		}
		config := util.BuildClientConfig(*clientData.Client, server, globalSettings)

		// create io reader from string
		reader := strings.NewReader(config)

		// set response header for downloading
		c.Response().Header().Set(echo.HeaderContentDisposition, fmt.Sprintf("attachment; filename=%s.conf", clientData.Client.Name))
		return c.Stream(http.StatusOK, "text/conf", reader)
	}
}

// RemoveClient handler
func RemoveClient(db store.IStore) echo.HandlerFunc {
	return func(c echo.Context) error {
		client := new(model.Client)
		c.Bind(client)

		if _, err := xid.FromString(client.ID); err != nil {
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Please provide a valid client ID"})
		}

		// delete client from database

		if err := db.DeleteClient(client.ID); err != nil {
			log.Error("Cannot delete wireguard client: ", err)
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot delete client from database"})
		}

		log.Infof("Removed wireguard client: %v", client)
		return c.JSON(http.StatusOK, jsonHTTPResponse{true, "Client removed"})
	}
}

// TerminateClient handler to terminate a client connection
func TerminateClient(db store.IStore, tmplDir fs.FS) echo.HandlerFunc {
	return func(c echo.Context) error {
		// Parse request body
		data := make(map[string]interface{})
		if err := json.NewDecoder(c.Request().Body).Decode(&data); err != nil {
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Bad post data"})
		}

		clientID := data["id"].(string)
		if _, err := xid.FromString(clientID); err != nil {
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Please provide a valid client ID"})
		}

		// Get client data
		clientData, err := db.GetClientByID(clientID, model.QRCodeSettings{Enabled: false})
		if err != nil {
			return c.JSON(http.StatusNotFound, jsonHTTPResponse{false, "Client not found"})
		}

		// Get settings for interface name
		settings, err := db.GetGlobalSettings()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot get global settings"})
		}

		// Get interface name from config file path or use default
		interfaceName := "wg0"
		if settings.ConfigFilePath != "" {
			parts := strings.Split(settings.ConfigFilePath, "/")
			if len(parts) > 0 {
				baseName := parts[len(parts)-1]
				interfaceName = strings.TrimSuffix(baseName, ".conf")
			}
		}

		// Create WireGuard client
		wgClient, err := wgctrl.New()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot create WireGuard client"})
		}
		defer wgClient.Close()

		// Parse public key
		pubKey, err := wgtypes.ParseKey(clientData.Client.PublicKey)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot parse client public key"})
		}

		// Remove peer from interface
		peerConfig := wgtypes.PeerConfig{
			PublicKey: pubKey,
			Remove:    true,
		}

		err = wgClient.ConfigureDevice(interfaceName, wgtypes.Config{
			Peers: []wgtypes.PeerConfig{peerConfig},
		})
		if err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, fmt.Sprintf("Cannot remove peer: %v", err)})
		}

		// Write new configuration
		server, err := db.GetServer()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot get server config"})
		}

		clients, err := db.GetClients(false)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot get clients"})
		}

		users, err := db.GetUsers()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot get users"})
		}

		err = util.WriteWireGuardServerConfig(tmplDir, server, clients, users, settings)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, fmt.Sprintf("Cannot write config: %v", err)})
		}

		log.Infof("Terminated client %s (%s)", clientData.Client.Name, clientData.Client.ID)
		return c.JSON(http.StatusOK, jsonHTTPResponse{true, "Client terminated successfully"})
	}
}

// WireGuardServer handler
func WireGuardServer(db store.IStore) echo.HandlerFunc {
	return func(c echo.Context) error {
		server, err := db.GetServer()
		if err != nil {
			log.Error("Cannot get server config: ", err)
		}

		return c.Render(http.StatusOK, "server.html", map[string]interface{}{
			"baseData":        model.BaseData{Active: "wg-server", CurrentUser: currentUser(c), Admin: isAdmin(c)},
			"serverInterface": server.Interface,
			"serverKeyPair":   server.KeyPair,
		})
	}
}

// WireGuardServerInterfaces handler
func WireGuardServerInterfaces(db store.IStore) echo.HandlerFunc {
	return func(c echo.Context) error {
		var serverInterface model.ServerInterface
		c.Bind(&serverInterface)

		// validate the input addresses
		if util.ValidateServerAddresses(serverInterface.Addresses) == false {
			log.Warnf("Invalid server interface addresses input from user: %v", serverInterface.Addresses)
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Interface IP address must be in CIDR format"})
		}

		serverInterface.UpdatedAt = time.Now().UTC()

		// write config to the database

		if err := db.SaveServerInterface(serverInterface); err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Interface IP address must be in CIDR format"})
		}
		log.Infof("Updated wireguard server interfaces settings: %v", serverInterface)

		return c.JSON(http.StatusOK, jsonHTTPResponse{true, "Updated interface addresses successfully"})
	}
}

// WireGuardServerKeyPair handler to generate private and public keys
func WireGuardServerKeyPair(db store.IStore) echo.HandlerFunc {
	return func(c echo.Context) error {
		// gen Wireguard key pair
		key, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			log.Error("Cannot generate wireguard key pair: ", err)
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot generate Wireguard key pair"})
		}

		var serverKeyPair model.ServerKeypair
		serverKeyPair.PrivateKey = key.String()
		serverKeyPair.PublicKey = key.PublicKey().String()
		serverKeyPair.UpdatedAt = time.Now().UTC()

		if err := db.SaveServerKeyPair(serverKeyPair); err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot generate Wireguard key pair"})
		}
		log.Infof("Updated wireguard server interfaces settings: %v", serverKeyPair)

		return c.JSON(http.StatusOK, serverKeyPair)
	}
}

// GlobalSettings handler
func GlobalSettings(db store.IStore) echo.HandlerFunc {
	return func(c echo.Context) error {
		globalSettings, err := db.GetGlobalSettings()
		if err != nil {
			log.Error("Cannot get global settings: ", err)
		}

		return c.Render(http.StatusOK, "global_settings.html", map[string]interface{}{
			"baseData":       model.BaseData{Active: "global-settings", CurrentUser: currentUser(c), Admin: isAdmin(c)},
			"globalSettings": globalSettings,
		})
	}
}

// Status handler
func Status(db store.IStore) echo.HandlerFunc {
	type PeerVM struct {
		Name              string
		Email             string
		PublicKey         string
		ReceivedBytes     int64
		TransmitBytes     int64
		LastHandshakeTime time.Time
		LastHandshakeRel  time.Duration
		Connected         bool
		AllocatedIP       string
		Endpoint          string
	}

	type DeviceVM struct {
		Name  string
		Peers []PeerVM
	}
	return func(c echo.Context) error {
		wgClient, err := wgctrl.New()
		if err != nil {
			return c.Render(http.StatusInternalServerError, "status.html", map[string]interface{}{
				"baseData": model.BaseData{Active: "status", CurrentUser: currentUser(c), Admin: isAdmin(c)},
				"error":    err.Error(),
				"devices":  nil,
			})
		}

		devices, err := wgClient.Devices()
		if err != nil {
			return c.Render(http.StatusInternalServerError, "status.html", map[string]interface{}{
				"baseData": model.BaseData{Active: "status", CurrentUser: currentUser(c), Admin: isAdmin(c)},
				"error":    err.Error(),
				"devices":  nil,
			})
		}

		devicesVm := make([]DeviceVM, 0, len(devices))
		if len(devices) > 0 {
			m := make(map[string]*model.Client)
			clients, err := db.GetClients(false)
			if err != nil {
				return c.Render(http.StatusInternalServerError, "status.html", map[string]interface{}{
					"baseData": model.BaseData{Active: "status", CurrentUser: currentUser(c), Admin: isAdmin(c)},
					"error":    err.Error(),
					"devices":  nil,
				})
			}
			for i := range clients {
				if clients[i].Client != nil {
					m[clients[i].Client.PublicKey] = clients[i].Client
				}
			}

			conv := map[bool]int{true: 1, false: 0}
			for i := range devices {
				devVm := DeviceVM{Name: devices[i].Name}
				for j := range devices[i].Peers {
					var allocatedIPs string
					for _, ip := range devices[i].Peers[j].AllowedIPs {
						if len(allocatedIPs) > 0 {
							allocatedIPs += "</br>"
						}
						allocatedIPs += ip.String()
					}
					pVm := PeerVM{
						PublicKey:         devices[i].Peers[j].PublicKey.String(),
						ReceivedBytes:     devices[i].Peers[j].ReceiveBytes,
						TransmitBytes:     devices[i].Peers[j].TransmitBytes,
						LastHandshakeTime: devices[i].Peers[j].LastHandshakeTime,
						LastHandshakeRel:  time.Since(devices[i].Peers[j].LastHandshakeTime),
						AllocatedIP:       allocatedIPs,
					}
					pVm.Connected = pVm.LastHandshakeRel.Minutes() < 3.

					if isAdmin(c) {
						pVm.Endpoint = devices[i].Peers[j].Endpoint.String()
					}

					if _client, ok := m[pVm.PublicKey]; ok {
						pVm.Name = _client.Name
						pVm.Email = _client.Email
					}
					devVm.Peers = append(devVm.Peers, pVm)
				}
				sort.SliceStable(devVm.Peers, func(i, j int) bool { return devVm.Peers[i].Name < devVm.Peers[j].Name })
				sort.SliceStable(devVm.Peers, func(i, j int) bool { return conv[devVm.Peers[i].Connected] > conv[devVm.Peers[j].Connected] })
				devicesVm = append(devicesVm, devVm)
			}
		}

		return c.Render(http.StatusOK, "status.html", map[string]interface{}{
			"baseData": model.BaseData{Active: "status", CurrentUser: currentUser(c), Admin: isAdmin(c)},
			"devices":  devicesVm,
			"error":    "",
		})
	}
}

// GlobalSettingSubmit handler to update the global settings
func GlobalSettingSubmit(db store.IStore) echo.HandlerFunc {
	return func(c echo.Context) error {
		var globalSettings model.GlobalSetting
		c.Bind(&globalSettings)

		// validate the input dns server list
		if util.ValidateIPAddressList(globalSettings.DNSServers) == false {
			log.Warnf("Invalid DNS server list input from user: %v", globalSettings.DNSServers)
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, "Invalid DNS server address"})
		}

		globalSettings.UpdatedAt = time.Now().UTC()

		// write config to the database
		if err := db.SaveGlobalSettings(globalSettings); err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot generate Wireguard key pair"})
		}

		log.Infof("Updated global settings: %v", globalSettings)

		return c.JSON(http.StatusOK, jsonHTTPResponse{true, "Updated global settings successfully"})
	}
}

// MachineIPAddresses handler to get local interface ip addresses
func MachineIPAddresses() echo.HandlerFunc {
	return func(c echo.Context) error {
		// get private ip addresses
		interfaceList, err := util.GetInterfaceIPs()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot get machine ip addresses"})
		}

		// get public ip address
		// TODO: Remove the go-external-ip dependency
		publicInterface, err := util.GetPublicIP()
		if err != nil {
			log.Warn("Cannot get machine public ip address: ", err)
		} else {
			// prepend public ip to the list
			interfaceList = append([]model.Interface{publicInterface}, interfaceList...)
		}

		return c.JSON(http.StatusOK, interfaceList)
	}
}

// GetOrderedSubnetRanges handler to get the ordered list of subnet ranges
func GetOrderedSubnetRanges() echo.HandlerFunc {
	return func(c echo.Context) error {
		return c.JSON(http.StatusOK, util.SubnetRangesOrder)
	}
}

// SuggestIPAllocation handler to get the list of ip address for client
func SuggestIPAllocation(db store.IStore) echo.HandlerFunc {
	return func(c echo.Context) error {
		server, err := db.GetServer()
		if err != nil {
			log.Error("Cannot fetch server config from database: ", err)
			return c.JSON(http.StatusBadRequest, jsonHTTPResponse{false, err.Error()})
		}

		// return the list of suggestedIPs
		// we take the first available ip address from
		// each server's network addresses.
		suggestedIPs := make([]string, 0)
		allocatedIPs, err := util.GetAllocatedIPs("")
		if err != nil {
			log.Error("Cannot suggest ip allocation. Failed to get list of allocated ip addresses: ", err)
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{
				false, "Cannot suggest ip allocation: failed to get list of allocated ip addresses",
			})
		}

		sr := c.QueryParam("sr")
		searchCIDRList := make([]string, 0)
		found := false

		// Use subnet range or default to interface addresses
		if util.SubnetRanges[sr] != nil {
			for _, cidr := range util.SubnetRanges[sr] {
				searchCIDRList = append(searchCIDRList, cidr.String())
			}
		} else {
			searchCIDRList = append(searchCIDRList, server.Interface.Addresses...)
		}

		// Save only unique IPs
		ipSet := make(map[string]struct{})

		for _, cidr := range searchCIDRList {
			ip, err := util.GetAvailableIP(cidr, allocatedIPs, server.Interface.Addresses)
			if err != nil {
				log.Error("Failed to get available ip from a CIDR: ", err)
				continue
			}
			found = true
			if strings.Contains(ip, ":") {
				ipSet[fmt.Sprintf("%s/128", ip)] = struct{}{}
			} else {
				ipSet[fmt.Sprintf("%s/32", ip)] = struct{}{}
			}
		}

		if !found {
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{
				false,
				"Cannot suggest ip allocation: failed to get available ip. Try a different subnet or deallocate some ips.",
			})
		}

		for ip := range ipSet {
			suggestedIPs = append(suggestedIPs, ip)
		}

		return c.JSON(http.StatusOK, suggestedIPs)
	}
}

// ApplyServerConfig handler to write config file and restart Wireguard server
func ApplyServerConfig(db store.IStore, tmplDir fs.FS) echo.HandlerFunc {
	return func(c echo.Context) error {
		server, err := db.GetServer()
		if err != nil {
			log.Error("Cannot get server config: ", err)
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot get server config"})
		}

		clients, err := db.GetClients(false)
		if err != nil {
			log.Error("Cannot get client config: ", err)
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot get client config"})
		}

		users, err := db.GetUsers()
		if err != nil {
			log.Error("Cannot get users config: ", err)
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot get users config"})
		}

		settings, err := db.GetGlobalSettings()
		if err != nil {
			log.Error("Cannot get global settings: ", err)
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{false, "Cannot get global settings"})
		}

		// Write config file
		err = util.WriteWireGuardServerConfig(tmplDir, server, clients, users, settings)
		if err != nil {
			log.Error("Cannot apply server config: ", err)
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{
				false, fmt.Sprintf("Cannot apply server config: %v", err),
			})
		}

		// Get interface name from config file path
		interfaceName := "wg0"
		if settings.ConfigFilePath != "" {
			parts := strings.Split(settings.ConfigFilePath, "/")
			if len(parts) > 0 {
				baseName := parts[len(parts)-1]
				interfaceName = strings.TrimSuffix(baseName, ".conf")
			}
		}

		// Restart WireGuard service
		serviceName := fmt.Sprintf("wg-quick@%s", interfaceName)
		cmd := exec.Command("sudo", "systemctl", "restart", serviceName)
		output, err := cmd.CombinedOutput()
		if err != nil {
			log.Error("Cannot restart WireGuard service: ", err, ", Output: ", string(output))
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{
				false, fmt.Sprintf("Cannot restart WireGuard service: %v", err),
			})
		}

		// Verify service is active
		checkCmd := exec.Command("sudo", "systemctl", "is-active", serviceName)
		status, err := checkCmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(status)) != "active" {
			log.Error("WireGuard service is not active after restart. Status: ", string(status))
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{
				false, "WireGuard service is not active after restart",
			})
		}

		err = util.UpdateHashes(db)
		if err != nil {
			log.Error("Cannot update hashes: ", err)
			return c.JSON(http.StatusInternalServerError, jsonHTTPResponse{
				false, fmt.Sprintf("Cannot update hashes: %v", err),
			})
		}

		return c.JSON(http.StatusOK, jsonHTTPResponse{true, "Applied server config successfully"})
	}
}

// GetHashesChanges handler returns if database hashes have changed
func GetHashesChanges(db store.IStore) echo.HandlerFunc {
	return func(c echo.Context) error {
		if util.HashesChanged(db) {
			return c.JSON(http.StatusOK, jsonHTTPResponse{true, "Hashes changed"})
		} else {
			return c.JSON(http.StatusOK, jsonHTTPResponse{false, "Hashes not changed"})
		}
	}
}

// AboutPage handler
func AboutPage() echo.HandlerFunc {
	return func(c echo.Context) error {
		return c.Render(http.StatusOK, "about.html", map[string]interface{}{
			"baseData": model.BaseData{Active: "about", CurrentUser: currentUser(c), Admin: isAdmin(c)},
		})
	}
}
