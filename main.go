package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
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
	ID        int    `json:"id"`
	Sender    string `json:"sender"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	System    bool   `json:"system"`
	Editable  bool   `json:"editable"`
}

type RoomMessage struct {
	Room    string
	Message Message
}

var upgrader = websocket.Upgrader{}
var rooms = make(map[string]map[*websocket.Conn]string)
var broadcast = make(chan RoomMessage)

func main() {
	var err error
	db, err = sql.Open("sqlite", "chat.db")
	if err != nil {
		panic(err)
	}
	db.SetMaxOpenConns(1)

	db.Exec("PRAGMA journal_mode=WAL")

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
	http.HandleFunc("/edit", editHandler)
	http.HandleFunc("/delete", deleteHandler)

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
		http.Error(w, "Missing fields", http.StatusBadRequest)
		return
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(creds.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "Hash error", http.StatusInternalServerError)
		return
	}
	_, err = db.Exec("INSERT INTO users (username, password_hash) VALUES (?, ?)", creds.Username, hashed)
	if err != nil {
		http.Error(w, "Conflict", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	var creds Credentials
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		http.Error(w, "Invalid", http.StatusBadRequest)
		return
	}
	var hash string
	err := db.QueryRow("SELECT password_hash FROM users WHERE username = ?", creds.Username).Scan(&hash)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(creds.Password)) != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	exp := time.Now().Add(time.Hour)
	claims := &Claims{Username: creds.Username, RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(exp)}}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokStr, err := token.SignedString(jwtKey)
	if err != nil {
		http.Error(w, "Token error", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"token": tokStr})
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	upgrader.CheckOrigin = func(r *http.Request) bool { return true }
	upgrader.Subprotocols = []string{r.Header.Get("Sec-WebSocket-Protocol")}
	tokStr := r.Header.Get("Sec-WebSocket-Protocol")
	if tokStr == "" {
		http.Error(w, "Missing token", http.StatusUnauthorized)
		return
	}
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokStr, claims, func(token *jwt.Token) (interface{}, error) {
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
	rows, _ := db.Query("SELECT id, sender, content, timestamp, system FROM messages WHERE room = ? ORDER BY id ASC", room)
	defer rows.Close()
	for rows.Next() {
		var m Message
		var sys int
		_ = rows.Scan(&m.ID, &m.Sender, &m.Content, &m.Timestamp, &sys)
		m.System = sys == 1
		m.Editable = m.Sender == username && !m.System
		ws.WriteJSON(m)
	}
	entry := Message{Sender: "System", Content: fmt.Sprintf("%s が入室しました", username), Timestamp: time.Now().Format("15:04:05"), System: true}
	saveMessageWithRetry(room, entry)
	broadcast <- RoomMessage{Room: room, Message: entry}
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			delete(rooms[room], ws)
			exit := Message{Sender: "System", Content: fmt.Sprintf("%s が退室しました", username), Timestamp: time.Now().Format("15:04:05"), System: true}
			saveMessageWithRetry(room, exit)
			broadcast <- RoomMessage{Room: room, Message: exit}
			break
		}
		newMsg := Message{Sender: username, Content: string(msg), Timestamp: time.Now().Format("15:04:05"), System: false, Editable: true}
		id, err := insertMessageAndGetID(room, newMsg)
		if err == nil {
			newMsg.ID = id
			broadcast <- RoomMessage{Room: room, Message: newMsg}
		}
	}
}

func insertMessageAndGetID(room string, m Message) (int, error) {
	sys := 0
	if m.System {
		sys = 1
	}
	result, err := db.Exec("INSERT INTO messages (sender, content, timestamp, system, room) VALUES (?, ?, ?, ?, ?)", m.Sender, m.Content, m.Timestamp, sys, room)
	if err != nil {
		fmt.Println("Failed to save message:", err)
		return 0, err
	}
	id, err := result.LastInsertId()
	return int(id), err
}

func saveMessageWithRetry(room string, m Message) {
	for i := 0; i < 3; i++ {
		_, err := insertMessageAndGetID(room, m)
		if err == nil {
			return
		}
		fmt.Println("Retry saving message:", err)
		time.Sleep(100 * time.Millisecond)
	}
}

func handleMessages() {
	for {
		rm := <-broadcast
		for client := range rooms[rm.Room] {
			rm.Message.Editable = rooms[rm.Room][client] == rm.Message.Sender && !rm.Message.System
			client.WriteJSON(rm.Message)
		}
	}
}

func editHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID      int    `json:"id"`
		Token   string `json:"token"`
		Content string `json:"content"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(req.Token, claims, func(token *jwt.Token) (interface{}, error) {
		return jwtKey, nil
	})
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	res, err := db.Exec("UPDATE messages SET content = ? WHERE id = ? AND sender = ?", req.Content, req.ID, claims.Username)
	if err != nil {
		http.Error(w, "DB error", http.StatusInternalServerError)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func deleteHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	token := r.URL.Query().Get("token")
	id, _ := strconv.Atoi(idStr)
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(token, claims, func(token *jwt.Token) (interface{}, error) {
		return jwtKey, nil
	})
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	res, err := db.Exec("DELETE FROM messages WHERE id = ? AND sender = ?", id, claims.Username)
	if err != nil {
		http.Error(w, "DB error", http.StatusInternalServerError)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusOK)
}
