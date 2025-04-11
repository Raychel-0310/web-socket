package main

import (
	"fmt"
	"net/http"

	"github.com/gorilla/websocket"
)

type Client struct {
	conn *websocket.Conn
	name string
}

var upgrader = websocket.Upgrader{}
var clients = make(map[*websocket.Conn]*Client)
var broadcast = make(chan Message)

type Message struct {
	Sender  string `json:"sender"`
	Content string `json:"content"`
}

func handleConnections(w http.ResponseWriter, r *http.Request) {
	upgrader.CheckOrigin = func(r *http.Request) bool { return true }
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Println("Upgrade error:", err)
		return
	}
	defer ws.Close()

	name := r.URL.Query().Get("name")
	if name == "" {
		name = "anonymous"
	}

	client := &Client{conn: ws, name: name}
	clients[ws] = client

	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			delete(clients, ws)
			break
		}
		broadcast <- Message{Sender: name, Content: string(msg)}
	}
}

func handleMessages() {
	for {
		msg := <-broadcast
		for _, client := range clients {
			client.conn.WriteJSON(msg)
		}
	}
}

func main() {
	fs := http.FileServer(http.Dir("./public"))
	http.Handle("/", fs)

	http.HandleFunc("/ws", handleConnections)

	go handleMessages()

	fmt.Println("Server started on :8080")
	http.ListenAndServe(":8080", nil)
}
