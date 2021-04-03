package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jonas747/dca"
	"github.com/pelletier/go-toml"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.in/gographics/imagick.v3/imagick"
)

type akuConfig struct {
	BaseURL string
	Port    int
	Token   string
}

const configPath = "config.toml"
const logPath = "/var/log/aku.log"
const convertedSoundCachePath = "/tmp/aku"
const stickerPageCachePath = "/tmp/akus"
const audioPath = "/var/go-aku/audio"
const stickerPath = "/var/go-aku/stickers"

type voiceChannelState struct {
	channel string
	guild   string
}

var config akuConfig
var userVoiceChannel map[string]voiceChannelState
var afkChannels map[string]string

var audioAssets map[string]string
var audioHelp map[string][]string
var audioBusy bool

var stickerAssets map[string]string
var stickerHelp map[string][]string
var stickerPackPages map[string][]string

const stringsPerPage = 10

const stickersPerRow = 4
const stickerRowsPerPage = 6

var stickersPerPage = stickersPerRow * stickerRowsPerPage

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

	// Read config
	configBytes, err := ioutil.ReadFile(configPath)
	if err != nil {
		log.Fatal().
			Err(err).
			Msg("Cannot find config file")
		os.Exit(1)
	}

	config = akuConfig{}
	toml.Unmarshal(configBytes, &config)

	// Initialize silly global state
	audioBusy = false
	activeHelpPages = make(map[string]helpPage)

	// Load assets
	audioAssets, audioHelp = loadAssets(audioPath)
	log.Info().
		Int("categories", len(audioHelp)).
		Int("sounds", len(audioAssets)).
		Msg("Loaded sounds")

	stickerAssets, stickerHelp = loadAssets(stickerPath)
	log.Info().
		Int("categories", len(stickerHelp)).
		Int("sounds", len(stickerAssets)).
		Msg("Loaded stickers")

	// Pre-cache entry sounds
	initializeConvertedSoundCache(getAssetPathsForCategory(audioAssets, audioHelp["entries"]))
	defer cleanUpCache(convertedSoundCachePath)

	// Pre-cache sticker pages
	packNames := getMapKeys(&stickerHelp)
	initializeStickerPageCache(packNames)
	defer cleanUpCache(stickerPageCachePath)

	// Static file server for message embeds
	// Wow this is a dumb idea
	log.Info().
		Int("port", config.Port).
		Str("baseURL", config.BaseURL).
		Msg("Running http server")
	fileServer := http.FileServer(http.Dir(stickerPageCachePath))
	http.Handle("/", fileServer)
	go func() {
		err := http.ListenAndServe(fmt.Sprintf(":%d", config.Port), nil)
		if err != nil {
			log.Fatal().
				Err(err).
				Msg("Static file server stopped")
			os.Exit(1)
		}
	}()

	// Make Discord session
	log.Info().Str("token", config.Token).Msg("Using token")
	dg, err := discordgo.New("Bot " + config.Token)
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

func getAssetPathsForCategory(assetPaths map[string]string, categoryAssets []string) map[string]string {
	targetAssetPaths := make(map[string]string)
	for _, assetName := range categoryAssets {
		targetAssetPaths[assetName] = assetPaths[assetName]
	}
	return targetAssetPaths
}

func initializeConvertedSoundCache(initialSounds map[string]string) {
	err := ensureDirectory(convertedSoundCachePath)
	if err != nil {
		log.Fatal().
			Err(err).
			Msg("Failed to create sound cache directory")

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

func getPageRange(page int, totalResults int, resultsPerPage int) (int, int) {
	pageStart := page * resultsPerPage
	pageEnd := (page + 1) * resultsPerPage
	if pageEnd > totalResults {
		pageEnd = totalResults
	}
	return pageStart, pageEnd
}

func renderPaginatedStrings(title string, allContents []string) func(int) (discordgo.MessageEmbed, error) {
	return func(page int) (discordgo.MessageEmbed, error) {
		pageStart, pageEnd := getPageRange(page, len(allContents), stringsPerPage)

		messageContent := ""
		for _, pageEntry := range allContents[pageStart:pageEnd] {
			messageContent += pageEntry + "\n"
		}

		footer := discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("Page %d/%d\n", page+1, totalPages(len(allContents), stringsPerPage)),
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
		totalPages: totalPages(len(sounds), stringsPerPage),
		renderPage: renderPaginatedStrings(category, sounds),
	}, nil
}

func initializeCategoryRootHelpPage(name string, index *map[string][]string) (helpPage, error) {
	categories := getMapKeys(index)
	sort.Strings(categories)

	return helpPage{
		name:       name,
		page:       0,
		totalPages: totalPages(len(categories), stringsPerPage),
		renderPage: renderPaginatedStrings("Categories", categories),
	}, nil
}

func totalPages(totalResults int, resultsPerPage int) int {
	return int(math.Ceil(float64(totalResults) / float64(resultsPerPage)))
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

	case "!akus":
		var assetPath, assetExists = stickerAssets[argument]
		if !assetExists {
			return
		}
		sendSticker(session, message.ChannelID, assetPath)

	case "!akush":
		sendStickerHelp(session, message.ChannelID, argument)
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

func ensureDirectory(directoryPath string) error {
	cacheDir, err := os.Stat(directoryPath)
	if os.IsNotExist(err) {
		// Create the cache directory if it doesn't exist
		if err := os.MkdirAll(directoryPath, 0700); err != nil {
			return errors.New("Failed to create directory")
		}
	} else if err != nil {
		return errors.New("Error statting path")
	} else if !cacheDir.IsDir() {
		return errors.New("Directory is a file")
	}
	return nil
}

func cleanUpCache(cachePath string) {
	err := os.RemoveAll(cachePath)
	if err != nil {
		log.Error().
			Str("cachePath", cachePath).
			Msg("Failed to remove cache directory")
	}
}

func getStickerPageFilename(packName string, page int) string {
	return fmt.Sprintf("%s-%d.png", packName, page)
}

func getStickerPagePath(packName string, page int) string {
	return filepath.Join(stickerPageCachePath, getStickerPageFilename(packName, page))
}

func getStickerPackPagePermalink(packName string, page int) string {
	return config.BaseURL + getStickerPageFilename(packName, page)
}

func initializeStickerPageCache(packNames []string) {
	stickerPackPages = make(map[string][]string)

	err := ensureDirectory(stickerPageCachePath)
	if err != nil {
		log.Fatal().
			Err(err).
			Msg("Failed to create sticker page cache directory")

		return
	}

	for _, packName := range packNames {
		log.Info().
			Str("packName", packName).
			Msg("Creating sticker pack pages")
		cacheStickerPackPages(packName)
	}
}

func cacheStickerPackPages(packName string) {
	packContents := getDirectoryContents(filepath.Join(stickerPath, packName))
	totalStickers := len(packContents)
	packPages := totalPages(totalStickers, stickersPerRow*stickerRowsPerPage)

	stickerPackPages[packName] = make([]string, packPages)
	for page := 0; page < packPages; page++ {
		pagePath := getStickerPagePath(packName, page)
		pageStart, pageEnd := getPageRange(page, totalStickers, stickersPerPage)
		makeStickerPackPage(packContents[pageStart:pageEnd], pagePath)
		stickerPackPages[packName][page] = pagePath
	}
}

func getDirectoryContents(directory string) []string {
	files, _ := ioutil.ReadDir(directory)
	paths := make([]string, 0, len(files))
	for _, f := range files {
		name := f.Name()
		if name != "sources" {
			paths = append(paths, filepath.Join(directory, name))
		}
	}
	return paths
}

func makeStickerPackPage(stickerPaths []string, outPath string) {
	imagick.Initialize()
	defer imagick.Terminate()

	mw := imagick.NewMagickWand()
	defer mw.Destroy()

	background := imagick.NewPixelWand()
	defer background.Destroy()
	background.SetColor("#ffffff")
	background.SetAlpha(0.0)
	mw.SetBackgroundColor(background)

	for _, stickerPath := range stickerPaths {
		mw.ReadImage(stickerPath)
	}

	// For each image we loaded
	mw.ResetIterator()
	for mw.NextImage() {
		// Give it a label which is the filename without the extension
		name := mw.GetImageFilename()
		label := strings.SplitN(filepath.Base(name), ".", 2)[0]
		_ = mw.LabelImage(label)
	}

	dw := imagick.NewDrawingWand()
	defer dw.Destroy()

	_ = dw.SetFont("./iosevka-aile-bold.ttf")
	dw.SetFontSize(14)

	montage := mw.MontageImage(
		dw,
		fmt.Sprintf("%dx%d+0+0", stickersPerRow, stickerRowsPerPage),
		"128x128+16+8",
		imagick.MONTAGE_MODE_UNFRAME,
		"0x0")
	_ = montage.BorderImage(background, 32, 16, imagick.COMPOSITE_OP_OVER)

	_ = montage.WriteImage(outPath)
}

func getMapKeys(index *map[string][]string) []string {
	// I can't believe golang makes you declare this by yourself
	// lol no generics
	keys := make([]string, 0, len(*index))
	for key := range *index {
		keys = append(keys, key)
	}
	return keys
}

func sendSticker(session *discordgo.Session, channelID string, stickerPath string) {
	stickerFile, err := os.Open(stickerPath)
	defer stickerFile.Close()
	if err != nil {
		log.Error().
			Err(err).
			Str("stickerPath", stickerPath).
			Msg("Error opening sticker file")
		return
	}
	session.ChannelFileSend(channelID, filepath.Base(stickerPath), stickerFile)
}

func initializeStickerHelpPage(packName string) (helpPage, error) {
	// Currently assumes that all pack pages are generated at startup.
	// Eventually, (if we had enough stickers) we might want to reduce startup tim
	// by generating sticker pages on-demand if the check here fails.
	packPages, found := stickerPackPages[packName]
	if !found {
		return helpPage{}, errors.New("Cached sticker pack pages not found")
	}

	totalPackPages := len(packPages)
	renderStickerPackPage := func(page int) (discordgo.MessageEmbed, error) {
		footer := discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("Page %d/%d\n", page+1, totalPackPages),
		}
		// So the discord API doesn't support multi-image embeds.
		// Instead we host a damn static file server, use an Image URL embed,
		// and swap out the URL when the page changes.
		// What even.
		permalink := getStickerPackPagePermalink(packName, page)
		log.Debug().
			Str("permalink", permalink).
			Msg("Sending sticker page embed")
		image := discordgo.MessageEmbedImage{
			URL: permalink,
		}
		return discordgo.MessageEmbed{
			Title:  packName,
			Image:  &image,
			Footer: &footer,
		}, nil
	}

	return helpPage{
		name:       "sticker/" + packName,
		page:       0,
		totalPages: totalPackPages,
		renderPage: renderStickerPackPage,
	}, nil
}

func sendStickerHelp(session *discordgo.Session, channelID string, category string) {
	var helpPage helpPage
	var err error
	if category == "" {
		helpPage, err = initializeCategoryRootHelpPage("sticker", &stickerHelp)
	} else {
		helpPage, err = initializeStickerHelpPage(category)
	}
	if err != nil {
		log.Info().
			Err(err).
			Str("category", category).
			Msg("Error initializing sticker help page")
		return
	}

	sendHelp(session, channelID, helpPage)
}
