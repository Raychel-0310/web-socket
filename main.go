package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
)

var jwtKey = []byte("secret_key")

var db *sql.DB

type Credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type Claims struct {
	Username string `json:"username"`
	jwt.RegisteredClaims
}

type Message struct {
	Sender    string `json:"sender"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	System    bool   `json:"system"`
}

type Client struct {
	conn     *websocket.Conn
	username string
	room     string
}

var upgrader = websocket.Upgrader{}
var rooms = make(map[string]map[*websocket.Conn]string)
var broadcast = make(chan RoomMessage)

type RoomMessage struct {
	Room    string
	Message Message
}

func main() {
	var err error
	db, err = sql.Open("sqlite", "chat.db")
	if err != nil {
		panic(err)
	}

	// Create messages table
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		sender TEXT,
		content TEXT,
		timestamp TEXT,
		system INTEGER,
		room TEXT
	)`)
	if err != nil {
		panic(err)
	}

	// Create users table
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT UNIQUE,
		password_hash TEXT
	)`)
	if err != nil {
		panic(err)
	}

	http.Handle("/", http.FileServer(http.Dir("./public")))
	http.HandleFunc("/register", registerHandler)
	http.HandleFunc("/login", loginHandler)
	http.HandleFunc("/ws", wsHandler)

	go handleMessages()

	fmt.Println("Server started on :8080")
	http.ListenAndServe(":8080", nil)
}

func registerHandler(w http.ResponseWriter, r *http.Request) {
	var creds Credentials
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	if creds.Username == "" || creds.Password == "" {
		http.Error(w, "Missing username or password", http.StatusBadRequest)
		return
	}

	// Hash the password
	hashed, err := bcrypt.GenerateFromPassword([]byte(creds.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "Could not hash password", http.StatusInternalServerError)
		return
	}

	_, err = db.Exec("INSERT INTO users (username, password_hash) VALUES (?, ?)", creds.Username, hashed)
	if err != nil {
		http.Error(w, "Username already exists or DB error", http.StatusConflict)
		return
	}

	w.WriteHeader(http.StatusCreated)
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	var creds Credentials
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	var storedHash string
	err := db.QueryRow("SELECT password_hash FROM users WHERE username = ?", creds.Username).Scan(&storedHash)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	err = bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(creds.Password))
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	expiration := time.Now().Add(1 * time.Hour)
	claims := &Claims{
		Username: creds.Username,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expiration),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(jwtKey)
	if err != nil {
		http.Error(w, "Could not create token", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"token": tokenString})
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	upgrader.CheckOrigin = func(r *http.Request) bool { return true }
	upgrader.Subprotocols = []string{r.Header.Get("Sec-WebSocket-Protocol")}

	tokenStr := r.Header.Get("Sec-WebSocket-Protocol")
	if tokenStr == "" {
		http.Error(w, "Missing token", http.StatusUnauthorized)
		return
	}

	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(token *jwt.Token) (interface{}, error) {
		return jwtKey, nil
	})
	if err != nil || !token.Valid {
		http.Error(w, "Invalid token", http.StatusUnauthorized)
		return
	}

	room := r.URL.Query().Get("room")
	if room == "" {
		room = "default"
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Println("Upgrade error:", err)
		return
	}
	defer ws.Close()

	username := claims.Username
	if rooms[room] == nil {
		rooms[room] = make(map[*websocket.Conn]string)
	}
	rooms[room][ws] = username

	rows, err := db.Query("SELECT sender, content, timestamp, system FROM messages WHERE room = ? ORDER BY id ASC", room)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var m Message
			var sys int
			if err := rows.Scan(&m.Sender, &m.Content, &m.Timestamp, &sys); err == nil {
				m.System = sys == 1
				ws.WriteJSON(m)
			}
		}
	}

	entry := Message{
		Sender:    "System",
		Content:   fmt.Sprintf("%s が入室しました", username),
		Timestamp: time.Now().Format("15:04:05"),
		System:    true,
	}
	broadcast <- RoomMessage{Room: room, Message: entry}
	saveMessage(room, entry)

	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			delete(rooms[room], ws)
			exit := Message{
				Sender:    "System",
				Content:   fmt.Sprintf("%s が退室しました", username),
				Timestamp: time.Now().Format("15:04:05"),
				System:    true,
			}
			broadcast <- RoomMessage{Room: room, Message: exit}
			saveMessage(room, exit)
			break
		}

		newMsg := Message{
			Sender:    username,
			Content:   string(msg),
			Timestamp: time.Now().Format("15:04:05"),
			System:    false,
		}
		broadcast <- RoomMessage{Room: room, Message: newMsg}
		saveMessage(room, newMsg)
	}
}

func handleMessages() {
	for {
		rm := <-broadcast
		for client := range rooms[rm.Room] {
			client.WriteJSON(rm.Message)
		}
	}
}

func saveMessage(room string, m Message) {
	sys := 0
	if m.System {
		sys = 1
	}
	_, err := db.Exec("INSERT INTO messages (sender, content, timestamp, system, room) VALUES (?, ?, ?, ?, ?)",
		m.Sender, m.Content, m.Timestamp, sys, room)
	if err != nil {
		fmt.Println("Failed to save message:", err)
	}
}
