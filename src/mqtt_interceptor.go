package main

import (
	"context"
	"log"
)

// mqttInterceptorWorker filters MQTT messages based on a switch state.
// It forwards messages from inputChan to outputChan only if the switch is enabled.
// Discovery topics (ending in /config) are always forwarded.
func mqttInterceptorWorker(
	ctx context.Context,
	name string,
	enableTopic string,
	inputChan <-chan MQTTMessage,
	outputChan chan<- MQTTMessage,
	dataChan <-chan DisplayData,
	forceEnable bool,
) {
	log.Printf("%s interceptor started\n", name)
	enabled := true // Default to enabled

	for {
		select {
		case data := <-dataChan:
			newEnabled := data.GetBoolean(enableTopic)
			if newEnabled != enabled {
				log.Printf("%s enabled: %v\n", name, newEnabled)
				enabled = newEnabled
			}

		case msg := <-inputChan:
			if forceEnable || enabled || isDiscoveryTopic(msg.Topic) {
				outputChan <- msg
			} else {
				log.Printf("%s disabled, dropping message to %s\n", name, msg.Topic)
			}

		case <-ctx.Done():
			log.Printf("%s interceptor stopped\n", name)
			return
		}
	}
}
