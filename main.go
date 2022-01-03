package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/fsnotify/fsnotify"
	"github.com/jonas747/dca"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const rootDir = "/go-aku"
const convertedSoundCachePath = "/go-aku/cache"
const audioPath = "/go-aku/audio"
const stickerPath = "/go-aku/stickers"

type voiceChannelState struct {
	channel string
	guild   string
}

var userVoiceChannel map[string]voiceChannelState
var afkChannels map[string]string

var audioAssets map[string]string
var audioHelp map[string][]string
var audioBusy bool

const resultsPerPage = 10
const previousPageEmoji = "⬅️"
const nextPageEmoji = "➡️"

var paginationReactions = []string{previousPageEmoji, nextPageEmoji}

type helpPage struct {
	name       string
	page       int
	totalPages int
	renderPage func(int) (discordgo.MessageEmbed, error)
}

var activeHelpPages map[string]helpPage

func main() {
	consoleWriter := zerolog.ConsoleWriter{Out: os.Stdout}

	log.Logger = zerolog.New(consoleWriter).With().Timestamp().Logger()
	zerolog.SetGlobalLevel(zerolog.InfoLevel)

	// Fetch token
	token := os.Getenv("DISCORD_TOKEN")

	// Initialize silly global state
	audioBusy = false
	activeHelpPages = make(map[string]helpPage)

	// Load assets
	audioAssets, audioHelp = loadAssets(audioPath)
	log.Info().
		Int("categories", len(audioHelp)).
		Int("sounds", len(audioAssets)).
		Msg("Loaded sounds")

	// Pre-cache entry sounds
	initializeConvertedSoundCache(getAssetPathsForCategory(audioAssets, audioHelp["entries"]))

	// Watch sound directory
	go watchAssetDir(audioPath, audioAssets, audioHelp)

	// Make Discord session
	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatal().
			Err(err).
			Msg("Error creating Discord session")
		os.Exit(1)
	}

	// Add event handlers
	dg.AddHandler(onReady)
	dg.AddHandler(onMessage)
	dg.AddHandler(onVoiceStateUpdate)
	dg.AddHandler(onMessageReactionAdd)

	// Connect
	err = dg.Open()
	defer dg.Close()
	if err != nil {
		log.Fatal().
			Err(err).
			Msg("Error opening Discord session")
		os.Exit(1)
	}

	// Wait until ctrl+c
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)
	<-sc

	// Clean up converted sound cache
	err = os.RemoveAll(convertedSoundCachePath)
	if err != nil {
		log.Fatal().
			Err(err).
			Str("convertedSoundCachePath", convertedSoundCachePath).
			Msg("Failed to clean up converted sound cache")
		return
	}
}

func getUniqueUsername(user *discordgo.User) string {
	return user.Username + "#" + user.Discriminator
}

func getAssetFromCommand(command string) string {
	return strings.Replace(strings.TrimSpace(command), " ", "_", -1)
}

func getCommandFromMessage(message string) (string, string) {
	var parts = strings.SplitN(message, " ", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], getAssetFromCommand(parts[1])
}

func getNormalizedAssetName(assetPath string) string {
	return strings.TrimSuffix(assetPath, filepath.Ext(assetPath))
}

func loadAssets(assetPath string) (map[string]string, map[string][]string) {
	var assetMap = make(map[string]string)
	var helpMap = make(map[string][]string)

	assetDir, err := ioutil.ReadDir(assetPath)
	if err != nil {
		log.Error().
			Err(err).
			Str("assetPath", assetPath).
			Msg("Error reading categories")
		return nil, nil
	}

	for _, category := range assetDir {
		if category.IsDir() {
			categoryName := category.Name()

			categoryPath := filepath.Join(assetPath, category.Name())
			categoryDir, err := ioutil.ReadDir(categoryPath)
			if err != nil {
				log.Error().
					Err(err).
					Str("categoryPath", categoryPath).
					Msg("Error reading assets from category")
				continue
			}
			helpMap[categoryName] = make([]string, 0)
			for _, asset := range categoryDir {
				if !asset.IsDir() {
					var assetFileName = asset.Name()
					var assetName = getNormalizedAssetName(assetFileName)
					assetMap[assetName] = filepath.Join(categoryPath, assetFileName)
					helpMap[categoryName] = append(helpMap[categoryName], assetName)
				}
			}
		}
	}
	return assetMap, helpMap
}

func watchDir(dirPath string, onCreate func(string), onRemove func(string)) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Error().Err(err).Msg("Error starting watcher")
	}
	defer watcher.Close()

	done := make(chan bool)

	go func() {
		for event := range watcher.Events {
			if event.Op&fsnotify.Create == fsnotify.Create {
				onCreate(filepath.Base(event.Name))
			} else if event.Op&fsnotify.Remove == fsnotify.Remove {
				if event.Name == dirPath {
					break
				}
				onRemove(filepath.Base(event.Name))
			}
		}
		done <- true
	}()

	err = watcher.Add(dirPath)
	if err != nil {
		log.Error().Err(err).Str("dirPath", dirPath).Msg("Failed to watch")
	}

	<-done
}

func watchAssetDir(assetPath string, assetMap map[string]string, helpMap map[string][]string) {
	watchDir(assetPath, func(category string) {
		categoryPath := filepath.Join(assetPath, category)
		info, err := os.Stat(categoryPath)
		if err != nil {
			log.Error().Err(err).Str("categoryPath", categoryPath).Msg("Error statting category directory")
		}
		if info.IsDir() {
			log.Info().Str("category", category).Msg("Added category")

			helpMap[category] = make([]string, 0)
			go watchDir(categoryPath, func(assetFile string) {
				var assetName = getNormalizedAssetName(assetFile)
				log.Info().Str("assetName", assetName).Msg("Added asset")
				assetMap[assetName] = filepath.Join(categoryPath, assetFile)
				helpMap[category] = append(helpMap[category], assetName)
			}, func(assetFile string) {
				log.Info().Str("assetFile", assetFile).Msg("Removed asset")
			})
		} else {
			log.Warn().Str("assetPath", assetPath).Str("category", category).Msg("Unexpected file in category directory")
		}
	}, func(category string) {
		log.Info().Str("category", category).Msg("Category removed")
		_, inHelp := helpMap[category]
		if inHelp {
			delete(helpMap, category)
		}
	})
}

func getAssetPathsForCategory(assetPaths map[string]string, categoryAssets []string) map[string]string {
	targetAssetPaths := make(map[string]string)
	for _, assetName := range categoryAssets {
		targetAssetPaths[assetName] = assetPaths[assetName]
	}
	return targetAssetPaths
}

func initializeConvertedSoundCache(initialSounds map[string]string) {
	cacheDir, err := os.Stat(convertedSoundCachePath)
	if os.IsNotExist(err) {
		// Create the cache directory if it doesn't exist
		if err := os.MkdirAll(convertedSoundCachePath, 0700); err != nil {
			log.Fatal().
				Err(err).
				Msg("Failed to create sound cache directory")
		}
	} else if err != nil {
		log.Fatal().
			Err(err).
			Str("convertedSoundCachePath", convertedSoundCachePath).
			Msg("Error statting sound cache directory")
		return
	} else if !cacheDir.IsDir() {
		log.Fatal().
			Err(err).
			Str("convertedSoundCachePath", convertedSoundCachePath).
			Msg("Sound cache directory is a file")
		return
	} else {
		// Converted sound cache left over from last time
		err := os.RemoveAll(convertedSoundCachePath)
		if err != nil {
			log.Fatal().
				Err(err).
				Str("convertedSoundCachePath", convertedSoundCachePath).
				Msg("Failed to delete existing converted sound cache")
			return
		}

		if err := os.MkdirAll(convertedSoundCachePath, 0700); err != nil {
			log.Fatal().
				Err(err).
				Msg("Failed to create sound cache directory")
		}
	}

	for soundName, soundPath := range initialSounds {
		convertAndCache(soundName, soundPath)
	}
}

func convertAndCache(soundName string, originalSoundPath string) {
	encodeSession, err := dca.EncodeFile(originalSoundPath, dca.StdEncodeOptions)
	defer encodeSession.Cleanup()

	var encodedPath = getConvertedSoundCachePath(soundName)
	// TODO: A leftover cached file could already be present
	output, err := os.Create(encodedPath)
	if err != nil {
		log.Error().
			Err(err).
			Str("soundName", soundName).
			Msg("Failed to cache sound")
		return
	}

	if _, err := io.Copy(output, encodeSession); err != nil {
		log.Error().
			Err(err).
			Str("soundName", soundName).
			Msg("Failed to copy encoded sound")
		return
	}
}

func getConvertedSoundCachePath(soundName string) string {
	return filepath.Join(convertedSoundCachePath, fmt.Sprintf("%s.dca", soundName))
}

func isSoundCached(soundName string) bool {
	var convertedSoundPath = getConvertedSoundCachePath(soundName)
	var _, err = os.Stat(convertedSoundPath)
	if os.IsNotExist(err) {
		return false
	} else if err != nil {
		log.Error().
			Err(err).
			Str("soundName", soundName).
			Msg("Error looking up converted sound")
		return false
	}
	return true
}

func onReady(session *discordgo.Session, event *discordgo.Ready) {
	log.Info().
		Msg("Long ago in a distant land...")

	populateInitialVoiceState(session)
}

func renderPaginatedStrings(title string, allContents []string) func(int) (discordgo.MessageEmbed, error) {
	return func(page int) (discordgo.MessageEmbed, error) {
		pageStart := page * resultsPerPage
		pageEnd := (page + 1) * resultsPerPage
		if pageEnd > len(allContents) {
			pageEnd = len(allContents)
		}

		messageContent := ""
		for _, pageEntry := range allContents[pageStart:pageEnd] {
			messageContent += pageEntry + "\n"
		}

		footer := discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("Page %d/%d\n", page+1, totalPages(allContents)),
		}
		return discordgo.MessageEmbed{
			Title:       title,
			Description: messageContent,
			Footer:      &footer,
		}, nil
	}
}

func initializeAudioCategoryHelpPage(category string) (helpPage, error) {
	sounds, categoryFound := audioHelp[category]
	if !categoryFound {
		return helpPage{}, errors.New("No such category")
	}
	sort.Strings(sounds)

	return helpPage{
		name:       "audio/" + category,
		page:       0,
		totalPages: totalPages(sounds),
		renderPage: renderPaginatedStrings(category, sounds),
	}, nil
}

func initializeCategoryRootHelpPage(name string, index *map[string][]string) (helpPage, error) {
	categories := make([]string, 0, len(*index))
	for category := range *index {
		categories = append(categories, category)
	}
	sort.Strings(categories)

	return helpPage{
		name:       name,
		page:       0,
		totalPages: totalPages(categories),
		renderPage: renderPaginatedStrings("Categories", categories),
	}, nil
}

func totalPages(allContents []string) int {
	return int(math.Ceil(float64(len(allContents)) / float64(resultsPerPage)))
}

func initializeReactions(session *discordgo.Session, channelID string, messageID string, targetEmoji []string) {
	for _, emoji := range targetEmoji {
		err := session.MessageReactionAdd(channelID, messageID, emoji)
		if err != nil {
			log.Error().
				Err(err).
				Str("emoji", emoji).
				Str("channelID", channelID).
				Str("messageID", messageID).
				Msg("Error initializing reaction")
		}
	}
}

func resetReactions(session *discordgo.Session, channelID string, messageID string, targetEmoji []string) {
	for _, emoji := range targetEmoji {
		// Remove reactions that aren't from the bot
		reactingUsers, err := session.MessageReactions(channelID, messageID, emoji, 100, "", "")
		if err != nil {
			log.Error().
				Err(err).
				Str("emoji", emoji).
				Str("channelID", channelID).
				Str("messageID", messageID).
				Msg("Error getting reactions")
		} else {
			for _, reactingUser := range reactingUsers {
				if reactingUser.ID != session.State.User.ID {
					err := session.MessageReactionRemove(channelID, messageID, emoji, reactingUser.ID)
					if err != nil {
						log.Error().
							Err(err).
							Str("emoji", emoji).
							Str("channelID", channelID).
							Str("messageID", messageID).
							Msg("Error removing reaction")
					}
				}
			}
		}
	}
}

func sendHelp(session *discordgo.Session, channelID string, helpPage helpPage) {
	messageContent, err := helpPage.renderPage(helpPage.page)
	if err != nil {
		log.Info().
			Err(err).
			Str("name", helpPage.name).
			Msg("Error rendering help page")
		return
	}

	message, err := session.ChannelMessageSendEmbed(channelID, &messageContent)
	if err != nil {
		log.Error().
			Err(err).
			Str("channelID", channelID).
			Msg("Error sending help")
		return
	}

	initializeReactions(session, channelID, message.ID, paginationReactions)
	activeHelpPages[message.ID] = helpPage
}

func sendAudioHelp(session *discordgo.Session, channelID string, category string) {
	var helpPage helpPage
	var err error
	if category == "" {
		helpPage, err = initializeCategoryRootHelpPage("audio", &audioHelp)
	} else {
		helpPage, err = initializeAudioCategoryHelpPage(category)
	}
	if err != nil {
		log.Info().
			Err(err).
			Str("category", category).
			Msg("Error initializing audio help page")
		return
	}

	sendHelp(session, channelID, helpPage)
}

func playSound(session *discordgo.Session, soundName string, soundPath string, authorVoiceState voiceChannelState) {
	if audioBusy {
		log.Debug().
			Msg("Skipping sound because another is being played")
		return
	}

	audioBusy = true
	defer func() {
		audioBusy = false
	}()

	startTime := time.Now()
	if !isSoundCached(soundName) {
		convertAndCache(soundName, soundPath)
	}

	var convertedSoundPath = getConvertedSoundCachePath(soundName)
	assetFile, err := os.Open(convertedSoundPath)
	defer assetFile.Close()
	if err != nil {
		log.Error().
			Err(err).
			Str("soundName", soundName).
			Str("soundPath", soundPath).
			Msg("Failed to open cached converted sound")
		return
	}

	decoder := dca.NewDecoder(assetFile)
	if err != nil {
		log.Error().
			Err(err).
			Str("soundName", soundName).
			Str("soundPath", soundPath).
			Msg("Failed to decode")
		return
	}

	voiceConnection, err := session.ChannelVoiceJoin(
		authorVoiceState.guild,
		authorVoiceState.channel,
		false,
		false)
	defer func() {
		err = voiceConnection.Disconnect()
		if err != nil {
			log.Error().
				Err(err).
				Str("guild", authorVoiceState.guild).
				Str("channel", authorVoiceState.channel).
				Msg("Failed to disconnect from voice")
		} else {
			log.Info().
				Str("guild", authorVoiceState.guild).
				Str("channel", authorVoiceState.channel).
				Msg("Disconnected from voice")
		}
	}()
	if err != nil {
		log.Error().
			Err(err).
			Str("guild", authorVoiceState.guild).
			Str("channel", authorVoiceState.channel).
			Msg("Failed to join voice")
		return
	}

	done := make(chan error)
	dca.NewStream(decoder, voiceConnection, done)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	select {
	case <-ctx.Done():
		log.Warn().
			Str("guild", authorVoiceState.guild).
			Str("channel", authorVoiceState.channel).
			Msg("Timed out while streaming sound to voice")
		return
	case err := <-done:
		if err != nil && err != io.EOF {
			log.Error().
				Err(err).
				Str("soundName", soundName).
				Str("soundPath", soundPath).
				Msg("Streaming decoded sound failed")
			return
		}
	}

	duration := time.Since(startTime)
	log.Debug().
		Dur("duration", duration).
		Str("soundName", soundName).
		Str("soundPath", soundPath).
		Msg("E2E sound play time")
}

func onMessage(session *discordgo.Session, message *discordgo.MessageCreate) {
	// Ignore ourselves
	if message.Author.ID == session.State.User.ID {
		return
	}

	var command, argument = getCommandFromMessage(message.Content)
	var authorUsername = getUniqueUsername(message.Author)
	if !strings.HasPrefix(command, "!aku") {
		return
	}

	defer func() {
		err := recover()
		if err != nil {
			log.Error().
				Str("command", command).
				Msgf("Panic in processing command: %v", err)
			return
		}
	}()

	log.Info().
		Str("command", command).
		Str("argument", argument).
		Str("authorUsername", authorUsername).
		Msg("Processing command")

	switch command {
	case "!aku":
		// Validate we can send
		var authorVoiceState, authorVoiceStateFound = userVoiceChannel[authorUsername]
		var assetPath, assetExists = audioAssets[argument]
		if !authorVoiceStateFound ||
			authorVoiceState.channel == "" ||
			!assetExists ||
			message.GuildID != authorVoiceState.guild {
			return
		}
		playSound(session, argument, assetPath, authorVoiceState)

	case "!akuh":
		sendAudioHelp(session, message.ChannelID, argument)
	}
}

func populateInitialVoiceState(session *discordgo.Session) {
	userVoiceChannel = make(map[string]voiceChannelState)

	trackedGuilds := 0
	trackedUsers := 0
	for _, guild := range session.State.Guilds {
		// Initially set everyone in the guild to no channel
		log.Info().
			Str("guild", guild.ID).
			Int("members", guild.MemberCount).
			Msg("Initialized guild")
		// I'll just pretend that guilds with more than 1000 members don't exist
		members, err := session.GuildMembers(guild.ID, "", 1000)
		if err != nil {
			log.Error().
				Err(err).
				Str("guild", guild.ID).
				Msg("Failed to fetch guild members")
			continue
		}

		for _, member := range members {
			username := getUniqueUsername(member.User)
			// If we've already seen this person, skip them
			if _, hasVoiceState := userVoiceChannel[username]; hasVoiceState {
				continue
			}

			userVoiceChannel[username] = voiceChannelState{"", guild.ID}
			trackedUsers++
		}

		// Voice states only contains people currently in a voice channel
		for _, voiceState := range guild.VoiceStates {
			user, err := session.User(voiceState.UserID)
			if err != nil {
				continue
			}

			// If they do have a current voice state, we'll overwrite the blank entry we put before
			username := getUniqueUsername(user)
			userVoiceChannel[username] = voiceChannelState{voiceState.ChannelID, voiceState.GuildID}
		}
		trackedGuilds++
	}

	log.Info().
		Int("trackedUsers", trackedUsers).
		Int("trackedGuilds", trackedGuilds).
		Msg("Loaded voice state data")
}

func onVoiceStateUpdate(session *discordgo.Session, event *discordgo.VoiceStateUpdate) {
	// Ignore ourselves
	if event.UserID == session.State.User.ID {
		return
	}

	user, err := session.User(event.UserID)
	if err != nil {
		log.Debug().
			Msg("Failed to get user from voice state update")
		return
	}

	guild, err := session.Guild(event.GuildID)
	if err != nil {
		log.Debug().
			Str("userID", user.ID).
			Msg("Failed to get guild from voice state update")
		return
	}

	username := getUniqueUsername(user)
	previousVoiceChannel := userVoiceChannel[username]
	newVoiceState := voiceChannelState{event.ChannelID, event.GuildID}
	userVoiceChannel[username] = newVoiceState
	log.Info().
		Str("username", username).
		Str("channelID", event.ChannelID).
		Str("guildID", event.GuildID).
		Str("previousChannel", previousVoiceChannel.channel).
		Str("previousGuild", previousVoiceChannel.guild).
		Msg("Voice state change")

	entrySoundPath, found := audioAssets[username]
	if !found {
		log.Info().
			Str("username", username).
			Msg("Don't have entry sound for user")
		// Don't have an entry sound for this user
		return
	}

	if newVoiceState.channel != "" && // Don't try to play sounds when the user leaves voice
		((previousVoiceChannel.channel == "") || // Just joined voice
			(guild.AfkChannelID != "" && previousVoiceChannel.channel == guild.AfkChannelID) || // Came back from AFK
			(previousVoiceChannel.guild != event.GuildID)) { // Came from a different guild
		log.Info().
			Str("channel", event.ChannelID).
			Str("guild", event.GuildID).
			Str("username", username).
			Msg("Playing entry sound")
		playSound(session, username, entrySoundPath, newVoiceState)
		log.Info().
			Str("channel", event.ChannelID).
			Str("guild", event.GuildID).
			Str("username", username).
			Msg("Played entry sound")
	}
}

func onMessageReactionAdd(session *discordgo.Session, event *discordgo.MessageReactionAdd) {
	if event.UserID == session.State.User.ID {
		return
	}

	// Always remove whatever reactions we got
	resetReactions(session, event.ChannelID, event.MessageID, paginationReactions)

	helpPage, found := activeHelpPages[event.MessageID]
	if !found {
		return
	}

	log.Debug().
		Str("messageID", event.MessageID).
		Str("emoji", event.Emoji.Name).
		Int("page", helpPage.page).
		Msg("Help page reaction")

	switch event.Emoji.Name {
	case previousPageEmoji:
		helpPage.page--
	case nextPageEmoji:
		helpPage.page++
	}

	if helpPage.page < 0 || helpPage.page >= helpPage.totalPages {
		// Page out of bounds, ignore
		return
	}

	// Track the page
	activeHelpPages[event.MessageID] = helpPage

	// Update the help message

	newHelpMessage, err := helpPage.renderPage(helpPage.page)

	_, err = session.ChannelMessageEditEmbed(event.ChannelID, event.MessageID, &newHelpMessage)
	if err != nil {
		log.Info().
			Err(err).
			Msg("Error changing help page")
		return
	}
}
