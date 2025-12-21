package main

import (
	"context"
	"fmt"
	"log"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// mqttWorker manages MQTT connection and forwards messages to a channel
func mqttWorker(
	ctx context.Context,
	broker string,
	topics []string,
	username, password, clientID string,
	msgChan chan<- SensorMessage,
	clientChan chan<- mqtt.Client,
) {
	// Connect to MQTT broker
	opts := mqtt.NewClientOptions()
	opts.AddBroker(fmt.Sprintf("tcp://%s:1883", broker))
	opts.SetClientID(clientID)
	opts.SetUsername(username)
	opts.SetPassword(password)
	opts.SetAutoReconnect(true)
	opts.SetConnectRetryInterval(5 * time.Second)

	// Set up connection lost handler
	opts.SetConnectionLostHandler(func(client mqtt.Client, err error) {
		log.Printf("MQTT connection lost: %v\n", err)
	})

	// Set up connection handler
	opts.SetOnConnectHandler(func(client mqtt.Client) {
		log.Printf("Connected to MQTT broker at %s\n", broker)

		// Send the new client to the sender worker
		select {
		case clientChan <- client:
			log.Println("Sent new MQTT client to sender worker")
		case <-ctx.Done():
			return
		}

		// Subscribe to all topics
		for _, topic := range topics {
			token := client.Subscribe(topic, 0, func(client mqtt.Client, msg mqtt.Message) {
				value := string(msg.Payload())

				// Skip invalid values from HA - sensor has dropped out
				// TODO: Track how long sensors have been invalid and send notification
				if value == "Undefined" || value == "unavailable" {
					return
				}

				// Forward message to stats worker via channel
				sensorMsg := SensorMessage{
					Topic: msg.Topic(),
					Value: value,
				}
				select {
				case msgChan <- sensorMsg:
				case <-ctx.Done():
					return
				}
			})

			if token.Wait() && token.Error() != nil {
				log.Printf("Failed to subscribe to topic %s: %v\n", topic, token.Error())
			} else {
				log.Printf("Subscribed to topic: %s\n", topic)
			}
		}
	})

	client := mqtt.NewClient(opts)

	// Connect to broker
	log.Printf("Connecting to MQTT broker at %s...\n", broker)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		log.Printf("Failed to connect to MQTT broker: %v\n", token.Error())
		return
	}

	// Keep worker alive until context is done
	<-ctx.Done()

	// Cleanup
	if client.IsConnected() {
		client.Disconnect(250)
		log.Println("Disconnected from MQTT broker")
	}
}
