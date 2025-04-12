package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
)

var jwtKey = []byte("secret_key")

type Credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type Claims struct {
	Username string `json:"username"`
	jwt.RegisteredClaims
}

type Message struct {
	Sender  string `json:"sender"`
	Content string `json:"content"`
}

type Client struct {
	conn     *websocket.Conn
	username string
	room     string
}

var upgrader = websocket.Upgrader{}
var rooms = make(map[string]map[*websocket.Conn]string) // room名 → conn → username
var broadcast = make(chan RoomMessage)

type RoomMessage struct {
	Room    string
	Message Message
}

func main() {
	http.Handle("/", http.FileServer(http.Dir("./public")))
	http.HandleFunc("/login", loginHandler)
	http.HandleFunc("/ws", wsHandler)

	go handleMessages()

	fmt.Println("Server started on :8080")
	http.ListenAndServe(":8080", nil)
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	var creds Credentials
	err := json.NewDecoder(r.Body).Decode(&creds)
	if err != nil || creds.Username == "" || creds.Password == "" {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	if creds.Password != "password123" {
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

	json.NewEncoder(w).Encode(map[string]string{
		"token": tokenString,
	})
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

	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			delete(rooms[room], ws)
			break
		}
		broadcast <- RoomMessage{
			Room: room,
			Message: Message{
				Sender:  username,
				Content: string(msg),
			},
		}
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
