package functions

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/gravitl/netclient/config"
	"github.com/gravitl/netclient/daemon"
	"github.com/gravitl/netclient/firewall"
	"github.com/gravitl/netclient/ncutils"
	"github.com/gravitl/netclient/networking"
	"github.com/gravitl/netclient/wireguard"
	"github.com/gravitl/netmaker/models"
	"golang.org/x/exp/slog"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// MQTimeout - time out for mqtt connections
const MQTimeout = 30

// All -- mqtt message hander for all ('#') topics
var All mqtt.MessageHandler = func(client mqtt.Client, msg mqtt.Message) {
	slog.Info("default message handler -- received message but not handling", "topic", msg.Topic())
}

// NodeUpdate -- mqtt message handler for /update/<NodeID> topic
func NodeUpdate(client mqtt.Client, msg mqtt.Message) {
	network := parseNetworkFromTopic(msg.Topic())
	slog.Info("processing node update for network", "network", network)
	node := config.GetNode(network)
	server := config.Servers[node.Server]
	data, err := decryptMsg(server.Name, msg.Payload())
	if err != nil {
		slog.Error("error decrypting message", "error", err)
		return
	}
	serverNode := models.Node{}
	if err = json.Unmarshal([]byte(data), &serverNode); err != nil {
		slog.Error("error unmarshalling node update data", "error", err)
		return
	}
	newNode := config.Node{}
	newNode.CommonNode = serverNode.CommonNode

	// see if cache hit, if so skip
	var currentMessage = read(newNode.Network, lastNodeUpdate)
	if currentMessage == string(data) {
		slog.Info("cache hit on node update ... skipping")
		return
	}
	insert(newNode.Network, lastNodeUpdate, string(data)) // store new message in cache
	slog.Info("received node update", "node", newNode.ID, "network", newNode.Network)
	// check if interface needs to delta
	ifaceDelta := wireguard.IfaceDelta(&node, &newNode)
	//nodeCfg.Node = newNode
	switch newNode.Action {
	case models.NODE_DELETE:
		slog.Info("received delete request for", "node", newNode.ID, "network", newNode.Network)
		unsubscribeNode(client, &newNode)
		if _, err = LeaveNetwork(newNode.Network, true); err != nil {
			if !strings.Contains("rpc error", err.Error()) {
				slog.Error("failed to leave network, please check that local files for network were removed", "network", newNode.Network, "error", err)
				return
			}
		}
		slog.Info("node was deleted", "node", newNode.ID, "network", newNode.Network)
		return
	case models.NODE_FORCE_UPDATE:
		ifaceDelta = true
	case models.NODE_NOOP:
	default:
	}
	// Save new config
	newNode.Action = models.NODE_NOOP
	config.UpdateNodeMap(network, newNode)
	if err := config.WriteNodeConfig(); err != nil {
		slog.Warn("failed to write node config", "error", err)
	}
	nc := wireguard.NewNCIface(config.Netclient(), config.GetNodes())
	if err := nc.Configure(); err != nil {
		slog.Error("could not configure netmaker interface", "error", err)
		return
	}
	time.Sleep(time.Second)
	if ifaceDelta { // if a change caused an ifacedelta we need to notify the server to update the peers
		doneErr := publishSignal(&newNode, DONE)
		if doneErr != nil {
			slog.Warn("could not notify server to update peers after interface change", "network:", newNode.Network, "error", doneErr)
		} else {
			slog.Info("signalled finished interface update to server", "network", newNode.Network)
		}
	}
}

// HostPeerUpdate - mq handler for host peer update peers/host/<HOSTID>/<SERVERNAME>
func HostPeerUpdate(client mqtt.Client, msg mqtt.Message) {
	var peerUpdate models.HostPeerUpdate
	var err error
	if len(config.GetNodes()) == 0 {
		slog.Info("skipping unwanted peer update, no nodes exist")
		return
	}
	serverName := parseServerFromTopic(msg.Topic())
	server := config.GetServer(serverName)
	if server == nil {
		slog.Error("server not found in config", "server", serverName)
		return
	}
	slog.Info("processing peer update for server", "server", serverName)
	data, err := decryptMsg(serverName, msg.Payload())
	if err != nil {
		slog.Error("error decrypting message", "error", err)
		return
	}
	err = json.Unmarshal([]byte(data), &peerUpdate)
	if err != nil {
		slog.Error("error unmarshalling peer data", "error", err)
		return
	}
	if server.IsPro && peerConnTicker != nil {
		peerConnTicker.Reset(peerConnectionCheckInterval)
	}
	if peerUpdate.ServerVersion != config.Version {
		slog.Warn("server/client version mismatch", "server", peerUpdate.ServerVersion, "client", config.Version)
		vlt, err := versionLessThan(config.Version, peerUpdate.ServerVersion)
		if err != nil {
			slog.Error("error checking version less than", "error", err)
			return
		}
		if vlt && config.Netclient().Host.AutoUpdate {
			slog.Info("updating client to server's version", "version", peerUpdate.ServerVersion)
			if err := UseVersion(peerUpdate.ServerVersion, false); err != nil {
				slog.Error("error updating client to server's version", "error", err)
			} else {
				slog.Info("updated client to server's version", "version", peerUpdate.ServerVersion)
				daemon.HardRestart()
			}
			//daemon.Restart()
		}
	}
	if peerUpdate.ServerVersion != server.Version {
		slog.Info("updating server version", "server", serverName, "version", peerUpdate.ServerVersion)
		server.Version = peerUpdate.ServerVersion
		config.WriteServerConfig()
	}
	config.UpdateHostPeers(peerUpdate.Peers)
	_ = config.WriteNetclientConfig()
	_ = wireguard.SetPeers(peerUpdate.ReplacePeers)
	if len(peerUpdate.EgressRoutes) > 0 {
		wireguard.SetEgressRoutes(peerUpdate.EgressRoutes)
	}
	go handleEndpointDetection(peerUpdate.Peers, peerUpdate.HostNetworkInfo)
	handleFwUpdate(serverName, &peerUpdate.FwUpdate)
}

// HostUpdate - mq handler for host update host/update/<HOSTID>/<SERVERNAME>
func HostUpdate(client mqtt.Client, msg mqtt.Message) {
	var hostUpdate models.HostUpdate
	var err error
	serverName := parseServerFromTopic(msg.Topic())
	server := config.GetServer(serverName)
	if server == nil {
		slog.Error("server not found in config", "server", serverName)
		return
	}
	if len(msg.Payload()) == 0 {
		return
	}
	data, err := decryptMsg(serverName, msg.Payload())
	if err != nil {
		slog.Error("error decrypting message", "error", err)
		return
	}
	err = json.Unmarshal([]byte(data), &hostUpdate)
	if err != nil {
		slog.Error("error unmarshalling host update data", "error", err)
		return
	}
	slog.Info("processing host update", "server", serverName, "action", hostUpdate.Action)
	var resetInterface, restartDaemon, sendHostUpdate, clearMsg bool
	switch hostUpdate.Action {
	case models.Upgrade:
		clearRetainedMsg(client, msg.Topic())
		cv, sv := config.Version, server.Version
		slog.Info("checking if need to upgrade client to server's version", "", config.Version, "version", server.Version)
		vlt, err := versionLessThan(cv, sv)
		if err == nil {
			// if we have an assertive result, and it's that
			// the client is up-to-date, nothing else to do
			if !vlt {
				slog.Info("no need to upgrade client, version is up-to-date")
				break
			}
		} else {
			// if we have a dubious result, assume that we need to upgrade client,
			// this can occur when using custom client versions not following semver
			slog.Warn("error checking version less than, but will proceed with upgrade", "error", err)
		}
		slog.Info("upgrading client to server's version", "version", sv)
		if err := UseVersion(sv, false); err != nil {
			slog.Error("error upgrading client to server's version", "error", err)
			break
		}
		slog.Info("upgraded client to server's version, restarting", "version", sv)
		if err := daemon.HardRestart(); err != nil {
			slog.Error("failed to hard restart daemon", "error", err)
		}
	case models.JoinHostToNetwork:
		commonNode := hostUpdate.Node.CommonNode
		nodeCfg := config.Node{
			CommonNode: commonNode,
		}
		config.UpdateNodeMap(hostUpdate.Node.Network, nodeCfg)
		server := config.GetServer(serverName)
		if server == nil {
			return
		}
		server.Nodes[hostUpdate.Node.Network] = true
		config.UpdateServer(serverName, *server)
		config.WriteNodeConfig()
		config.WriteServerConfig()
		slog.Info("added node to network", "network", hostUpdate.Node.Network, "server", serverName)
		clearRetainedMsg(client, msg.Topic()) // clear message before ACK
		if err = PublishHostUpdate(serverName, models.Acknowledgement); err != nil {
			slog.Error("failed to response with ACK to server", "server", serverName, "error", err)
		}
		setSubscriptions(client, &nodeCfg)
		resetInterface = true
	case models.DeleteHost:
		clearRetainedMsg(client, msg.Topic())
		unsubscribeHost(client, serverName)
		deleteHostCfg(client, serverName)
		config.WriteNodeConfig()
		config.WriteServerConfig()
		restartDaemon = true
	case models.UpdateHost:
		resetInterface, restartDaemon, sendHostUpdate = config.UpdateHost(&hostUpdate.Host)
		if sendHostUpdate {
			if err := PublishHostUpdate(config.CurrServer, models.UpdateHost); err != nil {
				slog.Error("could not publish host update", err.Error())
			}
		}
		clearMsg = true
	case models.RequestAck:
		clearRetainedMsg(client, msg.Topic()) // clear message before ACK
		if err = PublishHostUpdate(serverName, models.Acknowledgement); err != nil {
			slog.Error("failed to response with ACK to server", "server", serverName, "error", err)
		}
	case models.SignalHost:
		clearRetainedMsg(client, msg.Topic())
		processPeerSignal(hostUpdate.Signal)
	case models.UpdateKeys:
		clearRetainedMsg(client, msg.Topic()) // clear message
		UpdateKeys()
	case models.RequestPull:
		clearRetainedMsg(client, msg.Topic())
		Pull(true)
	default:
		slog.Error("unknown host action", "action", hostUpdate.Action)
		return
	}
	if err = config.WriteNetclientConfig(); err != nil {
		slog.Error("failed to write host config", "error", err)
		return
	}
	if restartDaemon {
		if clearMsg {
			clearRetainedMsg(client, msg.Topic())
		}
		if err := daemon.Restart(); err != nil {
			slog.Error("failed to restart daemon", "error", err)
		}
		return
	}
	if resetInterface {
		nc := wireguard.GetInterface()
		nc.Close()
		nc = wireguard.NewNCIface(config.Netclient(), config.GetNodes())
		nc.Create()
		if err := nc.Configure(); err != nil {
			slog.Error("could not configure netmaker interface", "error", err)
			return
		}
		if err = wireguard.SetPeers(false); err != nil {
			slog.Error("failed to set peers", err)
		}
	}
}

// handleEndpointDetection - select best interface for each peer and set it as endpoint
func handleEndpointDetection(peers []wgtypes.PeerConfig, peerInfo models.HostInfoMap) {
	currentCidrs := getAllAllowedIPs(peers[:])
	for idx := range peers {
		peerPubKey := peers[idx].PublicKey.String()
		if wireguard.EndpointDetectedAlready(peerPubKey) {
			continue
		}
		if peerInfo, ok := peerInfo[peerPubKey]; ok {
			if peerInfo.IsStatic {
				// peer is a static host shouldn't disturb the configuration set by the user
				continue
			}
			for i := range peerInfo.Interfaces {
				peerIface := peerInfo.Interfaces[i]
				peerIP := peerIface.Address.IP
				if peerIP == nil {
					continue
				}
				// check to skip bridge network
				if ncutils.IsBridgeNetwork(peerIface.Name) {
					continue
				}
				if strings.Contains(peerIP.String(), "127.0.0.") ||
					peerIP.IsMulticast() ||
					(peerIP.IsLinkLocalUnicast() && strings.Count(peerIP.String(), ":") >= 2) ||
					isAddressInPeers(peerIP, currentCidrs) {
					continue
				}
				if peerIP.IsPrivate() {
					go func(peerIP, peerPubKey string, listenPort int) {
						networking.FindBestEndpoint(
							peerIP,
							peerPubKey,
							peerInfo.ListenPort,
						)
					}(peerIP.String(), peerPubKey, peerInfo.ListenPort)

				}
			}
		}
	}
}

func deleteHostCfg(client mqtt.Client, server string) {
	config.DeleteServerHostPeerCfg()
	nodes := config.GetNodes()
	for k, node := range nodes {
		node := node
		if node.Server == server {
			unsubscribeNode(client, &node)
			config.DeleteNode(k)
		}
	}
	config.DeleteServer(server)
}

func parseNetworkFromTopic(topic string) string {
	return strings.Split(topic, "/")[2]
}

func parseServerFromTopic(topic string) string {
	return strings.Split(topic, "/")[3]
}

func getAllAllowedIPs(peers []wgtypes.PeerConfig) (cidrs []net.IPNet) {
	if len(peers) > 0 { // nil check
		for i := range peers {
			peer := peers[i]
			cidrs = append(cidrs, peer.AllowedIPs...)
		}
	}
	if cidrs == nil {
		cidrs = []net.IPNet{}
	}
	return
}

func isAddressInPeers(ip net.IP, cidrs []net.IPNet) bool {
	if len(cidrs) > 0 {
		for i := range cidrs {
			currCidr := cidrs[i]
			if currCidr.Contains(ip) {
				return true
			}
		}
	}
	return false
}

func handleFwUpdate(server string, payload *models.FwUpdate) {

	if payload.IsEgressGw {
		firewall.SetEgressRoutes(server, payload.EgressInfo)
	} else {
		firewall.DeleteEgressGwRoutes(server)
	}

}

// MQTT Fallback Mechanism
func mqFallback(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	mqFallbackTicker := time.NewTicker(time.Second * 30)
	for {
		select {
		case <-ctx.Done():
			mqFallbackTicker.Stop()
			slog.Info("mqfallback routine stop")
			return
		case <-mqFallbackTicker.C: // Execute pull every 30 seconds
			if (Mqclient != nil && Mqclient.IsConnectionOpen() && Mqclient.IsConnected()) || config.CurrServer == "" {
				continue
			}
			// Call netclient http config pull
			slog.Info("### mqfallback routine execute")
			response, resetInterface, replacePeers, err := Pull(false)
			if err != nil {
				slog.Error("pull failed", "error", err)
			} else {
				mqFallbackPull(response, resetInterface, replacePeers)
				server := config.GetServer(config.CurrServer)
				if server == nil {
					continue
				}
				slog.Info("re-attempt mqtt connection after pull")
				if Mqclient != nil {
					Mqclient.Disconnect(0)
				}
				if err := setupMQTT(server); err != nil {
					slog.Error("unable to connect to broker", "server", server.Broker, "error", err)
				}
			}

		}
	}
}

// MQTT Fallback Config Pull
func mqFallbackPull(pullResponse models.HostPull, resetInterface, replacePeers bool) {
	serverName := config.CurrServer
	server := config.GetServer(serverName)
	if server == nil {
		slog.Error("server not found in config", "server", serverName)
		return
	}
	if pullResponse.ServerConfig.Version != config.Version {
		slog.Warn("server/client version mismatch", "server", pullResponse.ServerConfig.Version, "client", config.Version)
		vlt, err := versionLessThan(config.Version, pullResponse.ServerConfig.Version)
		if err != nil {
			slog.Error("error checking version less than", "error", err)
			return
		}
		if vlt && config.Netclient().Host.AutoUpdate {
			slog.Info("updating client to server's version", "version", pullResponse.ServerConfig.Version)
			if err := UseVersion(pullResponse.ServerConfig.Version, false); err != nil {
				slog.Error("error updating client to server's version", "error", err)
			} else {
				slog.Info("updated client to server's version", "version", pullResponse.ServerConfig.Version)
				daemon.HardRestart()
			}
		}
	}
	if pullResponse.ServerConfig.Version != server.Version {
		slog.Info("updating server version", "server", serverName, "version", pullResponse.ServerConfig.Version)
		server.Version = pullResponse.ServerConfig.Version
		config.WriteServerConfig()
	}
	config.UpdateHostPeers(pullResponse.Peers)
	_ = config.WriteNetclientConfig()
	_ = wireguard.SetPeers(replacePeers)
	if len(pullResponse.EgressRoutes) > 0 {
		wireguard.SetEgressRoutes(pullResponse.EgressRoutes)
	}

	go handleEndpointDetection(pullResponse.Peers, pullResponse.HostNetworkInfo)
	handleFwUpdate(serverName, &pullResponse.FwUpdate)

	if resetInterface {
		nc := wireguard.GetInterface()
		nc.Close()
		nc = wireguard.NewNCIface(config.Netclient(), config.GetNodes())
		nc.Create()
		if err := nc.Configure(); err != nil {
			slog.Error("could not configure netmaker interface", "error", err)
			return
		}
		_ = wireguard.SetPeers(false)
		slog.Info("mqfallback reset interface")
	}
}
