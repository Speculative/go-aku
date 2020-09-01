package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"

	"github.com/bwmarrin/discordgo"
	"github.com/jonas747/dca"
)

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

func main() {
	// Read token
	var tokenBytes, err = ioutil.ReadFile("TOKEN")
	if err != nil {
		panic(fmt.Sprintf("Error reading token file: %v", err))
	}
	var token = string(tokenBytes)

	// Load assets
	audioAssets, audioHelp = loadAssets(audioPath)
	for categoryName, categoryContents := range audioHelp {
		fmt.Printf("%v:\n", categoryName)
		for _, asset := range categoryContents {
			fmt.Printf("\t%v\n", asset)
		}
	}

	// Make Discord session
	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		panic(fmt.Sprintf("Error creating Discord session: %v", err))
	}

	// Add event handlers
	dg.AddHandler(onReady)
	dg.AddHandler(onMessage)
	dg.AddHandler(onVoiceStateUpdate)

	// Connect
	err = dg.Open()
	defer dg.Close()
	if err != nil {
		fmt.Println("Error opening Discord session: ", err)
	}

	// Wait until ctrl+c
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)
	<-sc

	fmt.Println()
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
		panic(fmt.Sprintf("Error reading categories from %v: %v", assetPath, err))
	}

	for _, category := range assetDir {
		if category.IsDir() {
			var categoryName = category.Name()

			var categoryPath = path.Join(audioPath, category.Name())
			categoryDir, err := ioutil.ReadDir(categoryPath)
			if err != nil {
				panic(fmt.Sprintf("Error reading assets from %v: %v", categoryPath, err))
			}
			helpMap[categoryName] = make([]string, 0)
			for _, asset := range categoryDir {
				if !asset.IsDir() {
					var assetFileName = asset.Name()
					var assetName = getNormalizedAssetName(assetFileName)
					assetMap[assetName] = path.Join(categoryPath, assetFileName)
					helpMap[categoryName] = append(helpMap[categoryName], assetName)
				}
			}
		}
	}
	return assetMap, helpMap
}

func onReady(session *discordgo.Session, event *discordgo.Ready) {
	fmt.Println("Long ago in a distant land...")

	populateInitialVoiceState(session)
}

func sendHelp(session *discordgo.Session, targetUser *discordgo.User, helpMap map[string][]string, category string) {
	var username = getUniqueUsername(targetUser)
	var dmChannel, err = session.UserChannelCreate(targetUser.ID)
	if err != nil {
		panic(fmt.Sprintf("Error creating DM channel to %v: %v", username, err))
	}

	var messageContent = ""
	if category == "" {
		// List categories
		for key := range helpMap {
			messageContent += key + "\n"
		}
	} else {
		// List assets in category
		var categoryContents, categoryFound = helpMap[category]
		if !categoryFound {
			panic(fmt.Sprintf("Can't get help for category %v", category))
		}
		for _, asset := range categoryContents {
			messageContent += asset + "\n"
		}
	}
	_, err = session.ChannelMessageSend(dmChannel.ID, messageContent)
	if err != nil {
		panic(fmt.Sprintf("Error sending help to %v: %v", username, err))
	}
}

func playSound(session *discordgo.Session, assetPath string, authorVoiceState voiceChannelState) {
	encodeSession, err := dca.EncodeFile(assetPath, dca.StdEncodeOptions)
	if err != nil {
		panic(fmt.Sprintf("Failed to encode: %v", err))
	}
	defer encodeSession.Cleanup()

	voiceConnection, err := session.ChannelVoiceJoin(
		authorVoiceState.guild,
		authorVoiceState.channel,
		false,
		false)
	if err != nil {
		panic(fmt.Sprintf("Failed to join voice: %v", err))
	}
	defer func() {
		err = voiceConnection.Disconnect()
		if err != nil {
			panic(fmt.Sprintf("Failed to disconnect from voice: %v", err))
		}
	}()

	done := make(chan error)
	dca.NewStream(encodeSession, voiceConnection, done)
	err = <-done
	if err != nil && err != io.EOF {
		panic(fmt.Sprintf("Streaming encoded audio failed: %v", err))
	}
}

func onMessage(session *discordgo.Session, message *discordgo.MessageCreate) {
	// Ignore ourselves
	if message.Author.ID == session.State.User.ID {
		return
	}

	var command, argument = getCommandFromMessage(message.Content)
	var authorUsername = getUniqueUsername(message.Author)
	fmt.Printf("[%s] %s: %s\n", command, authorUsername, argument)

	defer func() {
		var r = recover()
		if r != nil {
			fmt.Printf("[%v]: %v\n", command, r)
		}
	}()

	switch command {
	case "!aku":
		// Validate we can send
		var authorVoiceState, authorVoiceStateFound = userVoiceChannel[authorUsername]
		var assetPath, assetExists = audioAssets[argument]
		if !authorVoiceStateFound || authorVoiceState.channel == "" || !assetExists || message.GuildID != authorVoiceState.guild {
			return
		}
		playSound(session, assetPath, authorVoiceState)

	case "!akuh":
		sendHelp(session, message.Author, audioHelp, argument)
	}
}

func populateInitialVoiceState(session *discordgo.Session) {
	userVoiceChannel = make(map[string]voiceChannelState)

	trackedGuilds := 0
	trackedUsers := 0
	for _, guild := range session.State.Guilds {
		// Initially set everyone in the guild to no channel
		fmt.Printf("[initialization] %s has %d members\n", guild.ID, len(guild.Members))
		// I'll just pretend that guilds with more than 1000 members don't exist
		members, err := session.GuildMembers(guild.ID, "", 1000)
		if err != nil {
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

	fmt.Printf("[initialization] loaded voice state data for %d users in %d guilds\n", trackedUsers, trackedGuilds)
}

func onVoiceStateUpdate(session *discordgo.Session, event *discordgo.VoiceStateUpdate) {
	user, err := session.User(event.UserID)
	if err != nil {
		return
	}

	guild, err := session.Guild(event.GuildID)
	if err != nil {
		return
	}

	username := getUniqueUsername(user)
	previousVoiceChannel := userVoiceChannel[username]
	userVoiceChannel[username] = voiceChannelState{event.ChannelID, event.GuildID}
	fmt.Printf("[voice-state] %s %s@%s, previously %s@%s\n", username, event.ChannelID, event.GuildID, previousVoiceChannel.channel, previousVoiceChannel.guild)

	if previousVoiceChannel.channel == "" || // Just joined voice
		(guild.AfkChannelID != "" && previousVoiceChannel.channel == guild.AfkChannelID) || // Came back from AFK
		(previousVoiceChannel.guild != event.GuildID) { // Came from a different guild
		fmt.Printf("[entry-sound] %s\n", username)
	}
}
