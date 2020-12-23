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
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jonas747/dca"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const logPath = "aku.log"
const convertedSoundCachePath = "/tmp/aku"
const audioPath = "audio"
const stickerPath = "stickers"

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
	index    *map[string][]string
	category string
	page     int
}

var activeHelpPages map[string]helpPage

func main() {
	// Set up logging
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("Failed to create log file: %v", err)
		os.Exit(1)
	}
	consoleWriter := zerolog.ConsoleWriter{Out: os.Stdout}
	multiWriter := zerolog.MultiLevelWriter(consoleWriter, logFile)

	log.Logger = zerolog.New(multiWriter).With().Timestamp().Logger()
	zerolog.SetGlobalLevel(zerolog.InfoLevel)

	// Read token
	tokenBytes, err := ioutil.ReadFile("TOKEN")
	if err != nil {
		log.Fatal().
			Err(err).
			Msg("Cannot find TOKEN file")
		os.Exit(1)
	}
	token := string(tokenBytes)

	// Initialize silly global state
	audioBusy = false
	activeHelpPages = make(map[string]helpPage)

	// Load assets
	audioAssets, audioHelp = loadAssets(audioPath)

	log.Info().
		Int("categories", len(audioHelp)).
		Int("sounds", len(audioAssets)).
		Msg("Loaded assets")

	initializeConvertedSoundCache(getAssetPathsForCategory(audioAssets, audioHelp["entries"]))

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
}

func getUniqueUsername(user *discordgo.User) string {
	return user.Username + "#" + user.Discriminator
}

func getAssetFromCommand(command string) string {
	return strings.Replace(strings.TrimSpace(command), " ", "_", 0)
}

func getCommandFromMessage(message string) (string, string) {
	var parts = strings.SplitN(message, " ", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], getAssetFromCommand(parts[1])
}

func getNormalizedAssetName(assetPath string) string {
	return strings.TrimSuffix(assetPath, path.Ext(assetPath))
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

func renderHelpPage(helpPage helpPage) (discordgo.MessageEmbed, error) {
	messageContent := ""
	var categoryContents []string
	if helpPage.category == "" {
		// List categories
		categoryContents = make([]string, 0, len(*helpPage.index))
		for category := range *helpPage.index {
			categoryContents = append(categoryContents, category)
		}
		sort.Strings(categoryContents)
	} else {
		// List assets in category
		assets, categoryFound := (*helpPage.index)[helpPage.category]
		if !categoryFound {
			log.Info().
				Str("category", helpPage.category).
				Msg("Non-existent category")
			return discordgo.MessageEmbed{}, errors.New("No such category")
		}
		sort.Strings(assets)
		categoryContents = assets
	}

	pageStart := helpPage.page * resultsPerPage
	pageEnd := (helpPage.page + 1) * resultsPerPage
	if pageEnd > len(categoryContents) {
		pageEnd = len(categoryContents)
	}
	for _, pageEntry := range categoryContents[pageStart:pageEnd] {
		messageContent += pageEntry + "\n"
	}

	title := helpPage.category
	if title == "" {
		title = "Categories"
	}
	footer := discordgo.MessageEmbedFooter{
		Text: fmt.Sprintf("Page %d/%d\n", helpPage.page+1, totalPages(helpPage.category, &audioHelp)),
	}
	return discordgo.MessageEmbed{
		Title:       title,
		Description: messageContent,
		Footer:      &footer,
	}, nil
}

func totalPages(category string, helpMap *map[string][]string) int {
	if category == "" {
		return int(math.Ceil(float64(len(*helpMap)) / float64(resultsPerPage)))
	}
	return int(math.Ceil(float64(len((*helpMap)[category])) / float64(resultsPerPage)))
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

func sendHelp(session *discordgo.Session, channelID string, index *map[string][]string, category string) {
	helpPage := helpPage{
		index:    index,
		category: category,
		page:     0,
	}
	messageContent, err := renderHelpPage(helpPage)
	if err != nil {
		log.Info().
			Err(err).
			Str("category", category).
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
		sendHelp(session, message.ChannelID, &audioHelp, argument)
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
	log.Debug().
		Str("username", username).
		Str("channelID", event.ChannelID).
		Str("guildID", event.GuildID).
		Str("previousChannel", previousVoiceChannel.channel).
		Str("previousGuild", previousVoiceChannel.guild).
		Msg("Voice state change")

	entrySoundPath, found := audioAssets[username]
	if !found {
		// Don't have an entry sound for this user
		return
	}

	if newVoiceState.channel != "" && // Don't try to play sounds when the user leaves voice
		((previousVoiceChannel.channel == "") || // Just joined voice
			(guild.AfkChannelID != "" && previousVoiceChannel.channel == guild.AfkChannelID) || // Came back from AFK
			(previousVoiceChannel.guild != event.GuildID)) { // Came from a different guild
		fmt.Printf("The weird channel is %v\n", newVoiceState.channel)
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

	log.Info().
		Str("messageID", event.MessageID).
		Str("emoji", event.Emoji.Name).
		Str("category", helpPage.category).
		Int("page", helpPage.page).
		Msg("Help page reaction")

	switch event.Emoji.Name {
	case previousPageEmoji:
		helpPage.page--
	case nextPageEmoji:
		helpPage.page++
	}

	if helpPage.page < 0 || helpPage.page >= totalPages(helpPage.category, &audioHelp) {
		// Page out of bounds, ignore
		return
	}

	// Track the page
	activeHelpPages[event.MessageID] = helpPage

	// Update the help message

	newHelpMessage, err := renderHelpPage(helpPage)

	_, err = session.ChannelMessageEditEmbed(event.ChannelID, event.MessageID, &newHelpMessage)
	if err != nil {
		log.Info().
			Err(err).
			Msg("Error changing help page")
		return
	}
}
