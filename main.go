package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/bwmarrin/discordgo"
)

const audioPath = "audio"
const stickerPath = "stickers"

type voiceChannelState struct {
	channel string
	guild   string
}

var userVoiceChannel map[string]voiceChannelState
var afkChannels map[string]string

func main() {
	// Make Discord session
	dg, err := discordgo.New("Bot " + "TOKEN GOES HERE")
	if err != nil {
		fmt.Println("Error creating Discord session: ", err)
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

func getAssetFromCommand(command string, prefix string) string {
	return strings.TrimSpace(strings.Replace(command, prefix, "", 1))
}

func onReady(session *discordgo.Session, event *discordgo.Ready) {
	fmt.Println("Long ago in a distant land...")

	populateInitialVoiceState(session)
}

func onMessage(session *discordgo.Session, message *discordgo.MessageCreate) {
	// Ignore ourselves
	if message.Author.ID == session.State.User.ID {
		return
	}

	if strings.HasPrefix(message.Content, "!aku") {
		assetName := getAssetFromCommand(message.Content, "!aku")
		fmt.Printf("[command-audio] %s: %s\n", getUniqueUsername(message.Author), message.Content)
		fmt.Printf("[audio-playing] %s\n", assetName)
	} else if strings.HasPrefix(message.Content, "!akus") {
		assetName := getAssetFromCommand(message.Content, "!akus")
		fmt.Printf("[command-sticker] %s: %s\n", getUniqueUsername(message.Author), message.Content)
		fmt.Printf("[sticker-showing] %s\n", assetName)
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
