package stride_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cosmos/cosmos-sdk/types"
	transfertypes "github.com/cosmos/ibc-go/v5/modules/apps/transfer/types"
	relayeribctest "github.com/cosmos/relayer/v2/ibctest"
	"github.com/cosmos/relayer/v2/ibctest/stride"
	ibctest "github.com/strangelove-ventures/ibctest/v5"
	"github.com/strangelove-ventures/ibctest/v5/chain/cosmos"
	"github.com/strangelove-ventures/ibctest/v5/ibc"
	"github.com/strangelove-ventures/ibctest/v5/test"
	"github.com/strangelove-ventures/ibctest/v5/testreporter"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"golang.org/x/sync/errgroup"
)

// TestStrideICAandICQ is a test case that performs simulations and assertions around interchain accounts
// and the client implementation of interchain queries. See: https://github.com/Stride-Labs/interchain-queries
func TestScenarioStrideICAandICQ(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	client, network := ibctest.DockerSetup(t)

	rep := testreporter.NewNopReporter()
	eRep := rep.RelayerExecReporter(t)

	ctx := context.Background()

	nf := 0
	nv := 1

	// Define chains involved in test
	cf := ibctest.NewBuiltinChainFactory(zaptest.NewLogger(t), []*ibctest.ChainSpec{
		{
			Name:          "stride",
			ChainName:     "stride",
			NumValidators: &nv,
			NumFullNodes:  &nf,
			ChainConfig: ibc.ChainConfig{
				Type:    "cosmos",
				Name:    "stride",
				ChainID: "stride-1",
				Images: []ibc.DockerImage{{
					Repository: "ghcr.io/strangelove-ventures/heighliner/stride",
					Version:    "andrew-test_admin_v5.1.1",
					UidGid:     "1025:1025",
				}},
				Bin:            "strided",
				Bech32Prefix:   "stride",
				Denom:          "ustrd",
				GasPrices:      "0.0ustrd",
				TrustingPeriod: TrustingPeriod,
				GasAdjustment:  1.1,
				ModifyGenesis:  ModifyGenesisStride(),
				EncodingConfig: stride.Encoding(),
			}},
		{
			Name:          "gaia",
			ChainName:     "gaia",
			Version:       "v8.0.0",
			NumValidators: &nv,
			NumFullNodes:  &nf,
			ChainConfig: ibc.ChainConfig{
				ModifyGenesis:  ModifyGenesisStrideCounterparty(),
				TrustingPeriod: TrustingPeriod,
			},
		},
	})

	chains, err := cf.Chains(t.Name())
	require.NoError(t, err)

	stride, gaia := chains[0].(*cosmos.CosmosChain), chains[1].(*cosmos.CosmosChain)
	strideCfg, gaiaCfg := stride.Config(), gaia.Config()

	r := relayeribctest.NewRelayer(t, relayeribctest.RelayerConfig{})

	// Build the network; spin up the chains and configure the relayer
	const pathStrideGaia = "stride-gaia"
	const relayerName = "relayer"

	clientOpts := ibc.DefaultClientOpts()
	clientOpts.TrustingPeriod = TrustingPeriod

	ic := ibctest.NewInterchain().
		AddChain(stride).
		AddChain(gaia).
		AddRelayer(r, relayerName).
		AddLink(ibctest.InterchainLink{
			Chain1:           stride,
			Chain2:           gaia,
			Relayer:          r,
			Path:             pathStrideGaia,
			CreateClientOpts: clientOpts,
		})

	require.NoError(t, ic.Build(ctx, eRep, ibctest.InterchainBuildOptions{
		TestName:  t.Name(),
		Client:    client,
		NetworkID: network,
		// Uncomment this to load blocks, txs, msgs, and events into sqlite db as test runs
		// BlockDatabaseFile: ibctest.DefaultBlockDatabaseFilepath(),

		SkipPathCreation: false,
	}))
	t.Cleanup(func() {
		_ = ic.Close()
	})

	// Fund user accounts, so we can query balances and make assertions.
	const userFunds = int64(10_000_000_000_000)
	users := ibctest.GetAndFundTestUsers(t, ctx, t.Name(), userFunds, stride, gaia)
	strideUser, gaiaUser := users[0], users[1]

	strideFullNode := stride.Validators[0]

	// Start the relayer
	err = r.StartRelayer(ctx, eRep, pathStrideGaia)
	require.NoError(t, err)

	t.Cleanup(
		func() {
			err := r.StopRelayer(ctx, eRep)
			if err != nil {
				t.Logf("an error occurred while stopping the relayer: %s", err)
			}
		},
	)

	// Recover stride admin key
	err = stride.RecoverKey(ctx, StrideAdminAccount, StrideAdminMnemonic)
	require.NoError(t, err)

	strideAdminAddrBytes, err := stride.GetAddress(ctx, StrideAdminAccount)
	require.NoError(t, err)

	strideAdminAddr, err := types.Bech32ifyAddressBytes(strideCfg.Bech32Prefix, strideAdminAddrBytes)
	require.NoError(t, err)

	err = stride.SendFunds(ctx, ibctest.FaucetAccountKeyName, ibc.WalletAmount{
		Address: strideAdminAddr,
		Amount:  userFunds,
		Denom:   strideCfg.Denom,
	})
	require.NoError(t, err, "failed to fund stride admin account")

	// get native chain user addresses
	strideAddr := strideUser.Bech32Address(strideCfg.Bech32Prefix)
	require.NotEmpty(t, strideAddr)

	gaiaAddress := gaiaUser.Bech32Address(gaiaCfg.Bech32Prefix)
	require.NotEmpty(t, gaiaAddress)

	// get ibc paths
	gaiaConns, err := r.GetConnections(ctx, eRep, gaiaCfg.ChainID)
	require.NoError(t, err)

	gaiaChans, err := r.GetChannels(ctx, eRep, gaiaCfg.ChainID)
	require.NoError(t, err)

	atomIBCDenom := transfertypes.ParseDenomTrace(
		transfertypes.GetPrefixedDenom(
			gaiaChans[0].Counterparty.PortID,
			gaiaChans[0].Counterparty.ChannelID,
			gaiaCfg.Denom,
		),
	).IBCDenom()

	var eg errgroup.Group

	// Fund stride user with ibc transfers
	gaiaHeight, err := gaia.Height(ctx)
	require.NoError(t, err)

	// Fund stride user with ibc denom atom
	tx, err := gaia.SendIBCTransfer(ctx, gaiaChans[0].ChannelID, gaiaUser.KeyName, ibc.WalletAmount{
		Amount:  1_000_000_000_000,
		Denom:   gaiaCfg.Denom,
		Address: strideAddr,
	}, nil)
	require.NoError(t, err)

	_, err = test.PollForAck(ctx, gaia, gaiaHeight, gaiaHeight+10, tx.Packet)
	require.NoError(t, err)

	require.NoError(t, eg.Wait())

	// Register gaia host zone
	_, err = strideFullNode.ExecTx(ctx, StrideAdminAccount,
		"stakeibc", "register-host-zone",
		gaiaConns[0].Counterparty.ConnectionId, gaiaCfg.Denom, gaiaCfg.Bech32Prefix,
		atomIBCDenom, gaiaChans[0].Counterparty.ChannelID, "1",
		"--gas", "1000000",
	)
	require.NoError(t, err)

	gaiaHeight, err = gaia.Height(ctx)
	require.NoError(t, err)

	// Wait for the ICA accounts to be setup
	_, err = PollForMsgChannelOpenConfirm(ctx, gaia, gaiaHeight, gaiaHeight+15, gaiaCfg.ChainID)
	require.NoError(t, err)

	// Get validator address
	gaiaVal1Address, err := gaia.Validators[0].KeyBech32(ctx, "validator", "val")
	require.NoError(t, err)

	// Add gaia validator
	_, err = strideFullNode.ExecTx(ctx, StrideAdminAccount,
		"stakeibc", "add-validator",
		gaiaCfg.ChainID, "gval1", gaiaVal1Address,
		"10", "5",
	)
	require.NoError(t, err)

	var gaiaHostZone HostZoneWrapper

	// query gaia host zone
	stdout, _, err := strideFullNode.ExecQuery(ctx,
		"stakeibc", "show-host-zone", gaiaCfg.ChainID,
	)
	require.NoError(t, err)
	err = json.Unmarshal(stdout, &gaiaHostZone)
	require.NoError(t, err)

	// Liquid stake some atom
	_, err = strideFullNode.ExecTx(ctx, strideUser.KeyName,
		"stakeibc", "liquid-stake",
		"1000000000000", gaiaCfg.Denom,
	)
	require.NoError(t, err)

	strideHeight, err := stride.Height(ctx)
	require.NoError(t, err)

	_, err = PollForMsgSubmitQueryResponse(ctx, stride, strideHeight, strideHeight+20, strideCfg.ChainID)
	require.NoError(t, err)
}