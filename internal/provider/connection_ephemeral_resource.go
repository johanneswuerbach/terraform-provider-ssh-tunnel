package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/johanneswuerbach/terraform-provider-sshtunnel/internal/portforward"
	"golang.org/x/crypto/ssh"
)

// Ensure provider defined types fully satisfy framework interfaces.
var _ ephemeral.EphemeralResource = &ConnectionEphemeralResource{}
var _ ephemeral.EphemeralResourceWithConfigure = &ConnectionEphemeralResource{}
var _ ephemeral.EphemeralResourceWithClose = &ConnectionEphemeralResource{}
var _ ephemeral.EphemeralResourceWithValidateConfig = &ConnectionEphemeralResource{}

func NewConnectionEphemeralResource() ephemeral.EphemeralResource {
	return &ConnectionEphemeralResource{}
}

// ConnectionEphemeralResource defines the resource implementation.
type ConnectionEphemeralResource struct {
	tunnelTracker *TunnelTracker
}

type ConnectionEphemeralResourceModelLocalPortForwarding struct {
	LocalPort     types.Int32  `tfsdk:"local_port"`
	RemoteHost    types.String `tfsdk:"remote_host"`
	RemotePort    types.Int32  `tfsdk:"remote_port"`
	RetryAttempts types.Int32  `tfsdk:"retry_attempts"`
	RetryDelay    types.String `tfsdk:"retry_delay"`
}

type ConnectionEphemeralResourceModelAuth struct {
	PrivateKey types.String `tfsdk:"private_key"`
}

// ConnectionEphemeralResourceModel describes the resource data model.
type ConnectionEphemeralResourceModel struct {
	Host                 types.String                                          `tfsdk:"host"`
	Port                 types.Int32                                           `tfsdk:"port"`
	User                 types.String                                          `tfsdk:"user"`
	Auth                 ConnectionEphemeralResourceModelAuth                  `tfsdk:"auth"`
	LocalPortForwardings []ConnectionEphemeralResourceModelLocalPortForwarding `tfsdk:"local_port_forwardings"`
}

const (
	connectionPrivateDataKey = "connection"
)

type ConnectionPrivateData struct {
	ID string
}

func (r *ConnectionEphemeralResource) Metadata(ctx context.Context, req ephemeral.MetadataRequest, resp *ephemeral.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_connection"
}

func (r *ConnectionEphemeralResource) Schema(ctx context.Context, req ephemeral.SchemaRequest, resp *ephemeral.SchemaResponse) {
	resp.Schema = schema.Schema{
		// This description is used by the documentation generator and the language server.
		MarkdownDescription: "The SSH Tunnel connection resource allows creating ephemeral SSH tunnels.",

		Attributes: map[string]schema.Attribute{
			"host": schema.StringAttribute{
				MarkdownDescription: "Host to connect to",
				Required:            true,
			},
			"port": schema.Int32Attribute{
				MarkdownDescription: "Port to connect to",
				Required:            true,
			},
			"user": schema.StringAttribute{
				MarkdownDescription: "User to connect as",
				Required:            true,
				Sensitive:           true,
			},
			"auth": schema.SingleNestedAttribute{
				MarkdownDescription: "Authentication details",
				Attributes: map[string]schema.Attribute{
					"private_key": schema.StringAttribute{
						MarkdownDescription: "Private key to use for authentication",
						Required:            true,
					},
				},
				Required:  true,
				Sensitive: true,
			},
			"local_port_forwardings": schema.ListNestedAttribute{
				MarkdownDescription: "Local port forwardings",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"local_port": schema.Int32Attribute{
							MarkdownDescription: "Local port to forward to (random if not specified)",
							Optional:            true,
							Computed:            true,
						},
						"remote_host": schema.StringAttribute{
							MarkdownDescription: "Remote host to forward to",
							Required:            true,
						},
						"remote_port": schema.Int32Attribute{
							MarkdownDescription: "Remote port to forward to",
							Required:            true,
						},
						"retry_attempts": schema.Int32Attribute{
							MarkdownDescription: "Number of attempts to establish the connection",
							Optional:            true,
						},
						"retry_delay": schema.StringAttribute{
							MarkdownDescription: "Delay between connection attempts",
							Optional:            true,
						},
					},
				},
				Required: true,
			},
		},
	}
}

func (r *ConnectionEphemeralResource) Configure(ctx context.Context, req ephemeral.ConfigureRequest, resp *ephemeral.ConfigureResponse) {
	// Always perform a nil check when handling ProviderData because Terraform
	// sets that data after it calls the ConfigureProvider RPC.
	if req.ProviderData == nil {
		return
	}

	configData, ok := req.ProviderData.(*ProviderConfigData)

	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Ephemeral Resource Configure Type",
			fmt.Sprintf("Expected *ProviderConfigData, got: %T. Please report this issue to the provider developers.", req.ProviderData),
		)

		return
	}

	r.tunnelTracker = configData.Tracker
}

func (r *ConnectionEphemeralResource) ValidateConfig(ctx context.Context, req ephemeral.ValidateConfigRequest, resp *ephemeral.ValidateConfigResponse) {
	var data ConnectionEphemeralResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	for _, localPortForwarding := range data.LocalPortForwardings {
		if !localPortForwarding.RetryDelay.IsNull() {
			if _, err := time.ParseDuration(localPortForwarding.RetryDelay.ValueString()); err != nil {
				resp.Diagnostics.AddError("Local Port Forwarding Error", fmt.Sprintf("Invalid retry delay: %s", err))
			}
		}
	}
}

func (r *ConnectionEphemeralResource) Open(ctx context.Context, req ephemeral.OpenRequest, resp *ephemeral.OpenResponse) {
	var data ConnectionEphemeralResourceModel

	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	id := randSeq(8)
	tunnelInfo := &TunnelInfo{}

	b, err := json.Marshal(&ConnectionPrivateData{ID: id})
	if err != nil {
		resp.Diagnostics.AddError("Private Data Error", fmt.Sprintf("Unable to marshal private data, got error: %s", err))
		return
	}
	resp.Private.SetKey(ctx, connectionPrivateDataKey, b)
	r.tunnelTracker.Add(id, tunnelInfo)

	// Setup SSH connection

	signer, err := ssh.ParsePrivateKey([]byte(data.Auth.PrivateKey.ValueString()))
	if err != nil {
		resp.Diagnostics.AddError("Private Key Error", fmt.Sprintf("Unable to parse private key, got error: %s", err))
		return
	}

	conn, err := ssh.Dial("tcp", hostAddr(data.Host, data.Port), &ssh.ClientConfig{
		User: data.User.ValueString(),
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Connection Error", fmt.Sprintf("Unable to connect to host %s, got error: %s", data.Host.ValueString(), err))
		return
	}

	tunnelInfo.conn = conn

	// Setup local port forwardings

	for i, localPortForwarding := range data.LocalPortForwardings {
		conf := &portforward.Config{
			LocalPort:  localPortForwarding.LocalPort.ValueInt32Pointer(),
			RemoteAddr: hostAddr(localPortForwarding.RemoteHost, localPortForwarding.RemotePort),
		}

		if !localPortForwarding.RetryDelay.IsNull() {
			retryDelay, err := time.ParseDuration(localPortForwarding.RetryDelay.ValueString())
			if err != nil {
				resp.Diagnostics.AddError("Local Port Forwarding Error", fmt.Sprintf("Invalid retry delay: %s", err))
				return
			}
			conf.RetryDelay = retryDelay
		}

		if !localPortForwarding.RetryAttempts.IsNull() {
			conf.RetryAttempts = localPortForwarding.RetryAttempts.ValueInt32()
		}

		listener, err := portforward.New(ctx, conn, conf)
		if err != nil {
			resp.Diagnostics.AddError("Port Forwarding Error", fmt.Sprintf("Unable to create port forwarding, got error: %s", err))
			resp.Diagnostics.Append(r.closeByConnectionID(id)...)
			return
		}
		tunnelInfo.listeners = append(tunnelInfo.listeners, listener)

		tcpAddr, ok := listener.Addr().(*net.TCPAddr)
		if !ok {
			resp.Diagnostics.AddError("Port Forwarding Error", "Listener address is not a TCP address")
			resp.Diagnostics.Append(r.closeByConnectionID(id)...)
			return
		}

		tflog.Info(ctx, "Port forwarding created", map[string]interface{}{
			"local_port": tcpAddr.Port,
		})

		data.LocalPortForwardings[i].LocalPort = basetypes.NewInt32Value(int32(tcpAddr.Port))
	}

	resp.Diagnostics.Append(resp.Result.Set(ctx, data)...)
}

func (r *ConnectionEphemeralResource) closeByConnectionID(id string) diag.Diagnostics {
	diags := diag.Diagnostics{}

	tunnelInfo := r.tunnelTracker.Get(id)
	if tunnelInfo == nil {
		return diags
	}

	for _, listener := range tunnelInfo.listeners {
		if err := listener.Close(); err != nil {
			diags.AddError("Failed to close listener", fmt.Sprintf("Failed to close listener: %v", err))
		}
	}

	if tunnelInfo.conn != nil {
		if err := tunnelInfo.conn.Close(); err != nil {
			diags.AddError("Failed to close connection", fmt.Sprintf("Failed to close connection: %v", err))
		}
	}

	r.tunnelTracker.Remove(id)

	return diags
}

func hostAddr(host basetypes.StringValue, port basetypes.Int32Value) string {
	return fmt.Sprintf("%s:%d", host.ValueString(), port.ValueInt32())
}

func (r *ConnectionEphemeralResource) Close(ctx context.Context, req ephemeral.CloseRequest, resp *ephemeral.CloseResponse) {
	b, diags := req.Private.GetKey(ctx, connectionPrivateDataKey)
	if diags.HasError() {
		resp.Diagnostics.Append(diags...)
		return
	}

	var privateData ConnectionPrivateData
	if err := json.Unmarshal(b, &privateData); err != nil {
		resp.Diagnostics.AddError("Private Data Error", fmt.Sprintf("Unable to unmarshal private data, got error: %s", err))
		return
	}

	resp.Diagnostics.Append(r.closeByConnectionID(privateData.ID)...)
}

var letters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

func randSeq(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}
