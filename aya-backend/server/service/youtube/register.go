package youtube_source

import (
	"aya-backend/server/service"
	"fmt"
	"sync"
	"time"

	"github.com/fatih/color"
	yt "google.golang.org/api/youtube/v3"
)

const (
	TIME_UNTIL_RETRY = 30 * time.Second
)

type youtubeRegister struct {
	mutex             sync.Mutex
	channelKillSignal map[string]chan bool
	apiCaller         *liveChatApiCaller
	ytService         *yt.Service
}

func newYoutubeRegister(ytService *yt.Service) *youtubeRegister {
	youtubeReg := youtubeRegister{
		channelKillSignal: make(map[string]chan bool),
		apiCaller:         newApiCaller(ytService),
		ytService:         ytService,
	}
	return &youtubeReg
}

func (register *youtubeRegister) deregisterChannel(channelId string) {
	register.mutex.Lock()
	defer register.mutex.Unlock()
	if register.channelKillSignal[channelId] == nil {
		// Don't have to do anything
		fmt.Printf("channel %s have not been registered, doing nothing\n", channelId)
		return
	}
	register.channelKillSignal[channelId] <- true
	close(register.channelKillSignal[channelId])
	delete(register.channelKillSignal, channelId)
	fmt.Printf("channel %s has been deregistered\n", channelId)
}

func (register *youtubeRegister) registerChannel(channelId string, msgChan chan service.MessageUpdate) {
	// attempt to get the channel info, i.e. is there any live vid at the moment

	register.mutex.Lock()
	defer register.mutex.Unlock()

	if register.ytService == nil {
		fmt.Printf("ytService not set up, skipping registering %s\n", channelId)
		return
	}

	if register.channelKillSignal[channelId] != nil {
		// Do not have to do anything, since it is already been registered
		fmt.Printf("channel %s have been registered, doing nothing\n", channelId)
		return
	}

	stopSignals := make(chan bool)
	register.channelKillSignal[channelId] = stopSignals
	errCh := make(chan error)
	stopDuringListening := make(chan bool)

	ytParser := YoutubeMessageParser{}

	setupChannel := func() {

		color.Yellow("setup channel %s\n", channelId)

		searchRes, err := register.ytService.Search.
			List([]string{"id"}).
			ChannelId(channelId).
			EventType("live").
			Type("video").
			Do()
		if err != nil {
			errCh <- err
			return
		}

		if len(searchRes.Items) == 0 {
			errCh <- fmt.Errorf("no live videos found for channel %s", channelId)
			return
		}

		videoId := searchRes.Items[0].Id.VideoId

		videoRes, err :=
			register.ytService.Videos.
				List([]string{"liveStreamingDetails"}).
				Id(videoId).
				Do()

		if err != nil {
			errCh <- err
			return
		}

		liveChatId := ""

		for _, item := range videoRes.Items {
			liveChatId = item.LiveStreamingDetails.ActiveLiveChatId
		}

		if liveChatId == "" {
			errCh <- fmt.Errorf("the live has ended")
			return
		}

		color.Green("Got the live video for channel %s", channelId)

		var pageToken *string

		for {
			liveChatMessagesService := yt.NewLiveChatMessagesService(register.ytService)
			liveChatServiceCall := liveChatMessagesService.List(liveChatId, []string{"snippet", "authorDetails"})
			if pageToken != nil {
				liveChatServiceCall = liveChatServiceCall.PageToken(*pageToken)
			}
			apiErrCh := make(chan error)
			responseCh := make(chan *yt.LiveChatMessageListResponse)
			liveChatApiRequest := liveChatAPIRequest{
				requestCall: liveChatServiceCall,
				responseCh:  responseCh,
				errCh:       apiErrCh,
			}
			color.Green("Calling liveChatApi")
			register.apiCaller.Request(liveChatApiRequest)
			select {
			case <-stopSignals:
				stopDuringListening <- true
				close(stopDuringListening)
				color.Red("stop signal during the stream of %s, stop", channelId)
				// wait for response from err and response before closing down
				select {
				case err := <-apiErrCh:
					errCh <- err
				case response := <-responseCh:
					for _, item := range response.Items {
						if item != nil && item.Snippet != nil {
							publishedTime, err := time.Parse(time.RFC3339, item.Snippet.PublishedAt)
							if err != nil {
								publishedTime = time.Now()
							}
							msgChan <- service.MessageUpdate{
								UpdateTime: publishedTime,
								Update:     service.New,
								Message:    ytParser.ParseMessage(item),
								ExtraFields: YoutubeInfo{
									YoutubeChannelId: channelId,
								},
							}
						}
					}
				}
				return
			case err := <-apiErrCh:
				fmt.Printf("Error during api calls: %v\n", err.Error())
				errCh <- err
				return
			case response := <-responseCh:
				pageToken = &response.NextPageToken
				for _, item := range response.Items {
					if item != nil && item.Snippet != nil {
						publishedTime, err := time.Parse(time.RFC3339, item.Snippet.PublishedAt)
						if err != nil {
							publishedTime = time.Now()
						}
						msgChan <- service.MessageUpdate{
							UpdateTime: publishedTime,
							Update:     service.New,
							Message:    ytParser.ParseMessage(item),
							ExtraFields: YoutubeInfo{
								YoutubeChannelId: channelId,
							},
						}
					}
				}
			}
		}
	}

	go func() {
		go setupChannel()
		for {
			select {
			case err := <-errCh:
				// sleep for a duration before a cool reset
				fmt.Printf("Error during processing channel %s: %s\nReseting in %s\n", channelId, err.Error(), TIME_UNTIL_RETRY)
				select {
				case <-time.After(TIME_UNTIL_RETRY):
					go setupChannel()
				case <-stopSignals:
					register.Stop()
					return
				}
			case <-stopDuringListening:
				color.Red("Stop when listening for channel %s. Return", channelId)
				return
			}
		}
	}()

	fmt.Printf("Finish register channel %s\n", channelId)
}

func (register *youtubeRegister) SetYTService(ytService *yt.Service) {
	register.ytService = ytService
	register.apiCaller.SetYTService(ytService)
}

func (register *youtubeRegister) Stop() {
	register.mutex.Lock()
	defer register.mutex.Unlock()
	for channelId, killSig := range register.channelKillSignal {
		killSig <- true
		color.Red("Kill Signal sent to channel %s", channelId)
		close(killSig)
	}
	register.apiCaller.Stop()
}
