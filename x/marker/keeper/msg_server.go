package keeper

import (
	"context"
	"fmt"

	"github.com/hashicorp/go-metrics"

	"cosmossdk.io/errors"
	sdkmath "cosmossdk.io/math"

	"github.com/cosmos/cosmos-sdk/telemetry"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"

	"github.com/provenance-io/provenance/x/marker/types"
)

type msgServer struct {
	Keeper
}

// NewMsgServerImpl returns an implementation of the marker MsgServer interface
// for the provided Keeper.
func NewMsgServerImpl(keeper Keeper) types.MsgServer {
	return &msgServer{Keeper: keeper}
}

var _ types.MsgServer = msgServer{}

// GrantAllowance grants an allowance from the marker's funds to be used by the grantee.
func (k msgServer) GrantAllowance(goCtx context.Context, msg *types.MsgGrantAllowanceRequest) (*types.MsgGrantAllowanceResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)
	m, err := k.GetMarkerByDenom(ctx, msg.Denom)
	if err != nil {
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}
	admin, err := sdk.AccAddressFromBech32(msg.Administrator)
	if err != nil {
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}
	grantee, err := sdk.AccAddressFromBech32(msg.Grantee)
	if err != nil {
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}
	if err = m.ValidateAddressHasAccess(admin, types.Access_Admin); err != nil {
		return nil, sdkerrors.ErrUnauthorized.Wrap(err.Error())
	}
	allowance, err := msg.GetFeeAllowanceI()
	if err != nil {
		return nil, err
	}
	err = k.Keeper.feegrantKeeper.GrantAllowance(ctx, m.GetAddress(), grantee, allowance)
	return &types.MsgGrantAllowanceResponse{}, err
}

// Handle a message to add a new marker account.
func (k msgServer) AddMarker(goCtx context.Context, msg *types.MsgAddMarkerRequest) (*types.MsgAddMarkerResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	isGovProp := msg.FromAddress == k.GetAuthority()

	var err error
	// If this isn't from a gov prop, there's some added restrictions to check.
	if !isGovProp {
		// Only Proposed and Finalized statuses are allowed.
		if msg.Status != types.StatusFinalized && msg.Status != types.StatusProposed {
			return nil, fmt.Errorf("marker can only be created with a Proposed or Finalized status")
		}
		// There's extra restrictions on the denom.
		if err = k.ValidateUnrestictedDenom(ctx, msg.Amount.Denom); err != nil {
			return nil, err
		}
	}

	addr := types.MustGetMarkerAddress(msg.Amount.Denom)
	var manager sdk.AccAddress
	switch {
	case msg.Manager != "":
		manager, err = sdk.AccAddressFromBech32(msg.Manager)
	case msg.Status != types.StatusActive:
		manager, err = sdk.AccAddressFromBech32(msg.FromAddress)
	}
	if err != nil {
		return nil, err
	}

	// If this is via gov prop, just use the provided AllowGovernanceControl value.
	// Otherwise, if either requested or governance is enabled in params, allow it.
	allowGovControl := msg.AllowGovernanceControl || (!isGovProp && k.GetEnableGovernance(ctx))

	normalizedReqAttrs, err := k.NormalizeRequiredAttributes(ctx, msg.RequiredAttributes)
	if err != nil {
		return nil, err
	}

	ma := types.NewMarkerAccount(
		authtypes.NewBaseAccountWithAddress(addr),
		sdk.NewCoin(msg.Amount.Denom, msg.Amount.Amount),
		manager,
		msg.AccessList,
		msg.Status,
		msg.MarkerType,
		msg.SupplyFixed,
		allowGovControl,
		msg.AllowForcedTransfer,
		normalizedReqAttrs,
	)

	if err = k.Keeper.AddMarkerAccount(ctx, ma); err != nil {
		ctx.Logger().Error("unable to add marker", "err", err)
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	// Only create a NAV entry if an explicit value is given for a NAV.  If a zero value is desired this can be set explicitly in a followup call.
	// This check prevents a proliferation of incorrect NAV entries being recorded when setting up markers.
	if msg.UsdMills > 0 {
		usdMills := sdkmath.NewIntFromUint64(msg.UsdMills)
		nav := types.NewNetAssetValue(sdk.NewCoin(types.UsdDenom, usdMills), msg.Volume)
		err = k.AddSetNetAssetValues(ctx, ma, []types.NetAssetValue{nav}, types.ModuleName)
		if err != nil {
			return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
		}
	}

	// Note: The status can only be Active if this is being done via gov prop.
	if ma.Status == types.StatusActive {
		// Active markers should have supply set.
		if err = k.AdjustCirculation(ctx, ma, msg.Amount); err != nil {
			return nil, err
		}
	}

	return &types.MsgAddMarkerResponse{}, nil
}

// AddAccess handles a message to grant access to a marker account.
func (k msgServer) AddAccess(goCtx context.Context, msg *types.MsgAddAccessRequest) (*types.MsgAddAccessResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	// Validate transaction message.
	if err := msg.ValidateBasic(); err != nil {
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	admin := sdk.MustAccAddressFromBech32(msg.Administrator)

	for i := range msg.Access {
		access := msg.Access[i]
		if err := k.Keeper.AddAccess(ctx, admin, msg.Denom, &access); err != nil {
			ctx.Logger().Error("unable to add access grant to marker", "err", err)
			return nil, sdkerrors.ErrUnauthorized.Wrap(err.Error())
		}
	}

	return &types.MsgAddAccessResponse{}, nil
}

// DeleteAccess handles a message to revoke access to marker account.
func (k msgServer) DeleteAccess(goCtx context.Context, msg *types.MsgDeleteAccessRequest) (*types.MsgDeleteAccessResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	// Validate transaction message.
	if err := msg.ValidateBasic(); err != nil {
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	admin := sdk.MustAccAddressFromBech32(msg.Administrator)
	addr := sdk.MustAccAddressFromBech32(msg.RemovedAddress)

	if err := k.Keeper.RemoveAccess(ctx, admin, msg.Denom, addr); err != nil {
		ctx.Logger().Error("unable to remove access grant from marker", "err", err)
		return nil, sdkerrors.ErrUnauthorized.Wrap(err.Error())
	}

	return &types.MsgDeleteAccessResponse{}, nil
}

// Finalize handles a message to finalize a marker
func (k msgServer) Finalize(goCtx context.Context, msg *types.MsgFinalizeRequest) (*types.MsgFinalizeResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	// Validate transaction message.
	if err := msg.ValidateBasic(); err != nil {
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	admin := sdk.MustAccAddressFromBech32(msg.Administrator)

	if err := k.Keeper.FinalizeMarker(ctx, admin, msg.Denom); err != nil {
		ctx.Logger().Error("unable to finalize marker", "err", err)
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	return &types.MsgFinalizeResponse{}, nil
}

// Activate handles a message to activate a marker
func (k msgServer) Activate(goCtx context.Context, msg *types.MsgActivateRequest) (*types.MsgActivateResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	// Validate transaction message.
	if err := msg.ValidateBasic(); err != nil {
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	admin := sdk.MustAccAddressFromBech32(msg.Administrator)

	if err := k.Keeper.ActivateMarker(ctx, admin, msg.Denom); err != nil {
		ctx.Logger().Error("unable to activate marker", "err", err)
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	return &types.MsgActivateResponse{}, nil
}

// Cancel handles a message to cancel a marker
func (k msgServer) Cancel(goCtx context.Context, msg *types.MsgCancelRequest) (*types.MsgCancelResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	// Validate transaction message.
	if err := msg.ValidateBasic(); err != nil {
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	admin := sdk.MustAccAddressFromBech32(msg.Administrator)

	if err := k.Keeper.CancelMarker(ctx, admin, msg.Denom); err != nil {
		ctx.Logger().Error("unable to cancel marker", "err", err)
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	return &types.MsgCancelResponse{}, nil
}

// Delete handles a message to delete a marker
func (k msgServer) Delete(goCtx context.Context, msg *types.MsgDeleteRequest) (*types.MsgDeleteResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	// Validate transaction message.
	if err := msg.ValidateBasic(); err != nil {
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	admin := sdk.MustAccAddressFromBech32(msg.Administrator)

	if err := k.Keeper.DeleteMarker(ctx, admin, msg.Denom); err != nil {
		ctx.Logger().Error("unable to delete marker", "err", err)
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	return &types.MsgDeleteResponse{}, nil
}

// Mint handles a message to mint additional supply for a marker.
func (k msgServer) Mint(goCtx context.Context, msg *types.MsgMintRequest) (*types.MsgMintResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	// Validate transaction message.
	if err := msg.ValidateBasic(); err != nil {
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	admin := sdk.MustAccAddressFromBech32(msg.Administrator)

	if err := k.Keeper.MintCoin(ctx, admin, msg.Amount); err != nil {
		ctx.Logger().Error("unable to mint coin for marker", "err", err)
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	defer func() {
		telemetry.IncrCounterWithLabels(
			[]string{types.ModuleName, types.EventTelemetryKeyMint},
			1,
			[]metrics.Label{
				telemetry.NewLabel(types.EventTelemetryLabelDenom, msg.Amount.GetDenom()),
				telemetry.NewLabel(types.EventTelemetryLabelAdministrator, msg.Administrator),
			},
		)
		if msg.Amount.Amount.IsInt64() {
			telemetry.SetGaugeWithLabels(
				[]string{types.ModuleName, types.EventTelemetryKeyMint, msg.Amount.Denom},
				float32(msg.Amount.Amount.Int64()),
				[]metrics.Label{telemetry.NewLabel(types.EventTelemetryLabelDenom, msg.Amount.Denom)},
			)
		}
	}()

	return &types.MsgMintResponse{}, nil
}

// Burn handles a message to burn coin from a marker account.
func (k msgServer) Burn(goCtx context.Context, msg *types.MsgBurnRequest) (*types.MsgBurnResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	// Validate transaction message.
	if err := msg.ValidateBasic(); err != nil {
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	admin := sdk.MustAccAddressFromBech32(msg.Administrator)

	if err := k.Keeper.BurnCoin(ctx, admin, msg.Amount); err != nil {
		ctx.Logger().Error("unable to burn coin from marker", "err", err)
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	defer func() {
		telemetry.IncrCounterWithLabels(
			[]string{types.ModuleName, types.EventTelemetryKeyBurn},
			1,
			[]metrics.Label{
				telemetry.NewLabel(types.EventTelemetryLabelDenom, msg.Amount.GetDenom()),
				telemetry.NewLabel(types.EventTelemetryLabelAdministrator, msg.Administrator),
			},
		)
		if msg.Amount.Amount.IsInt64() {
			telemetry.SetGaugeWithLabels(
				[]string{types.ModuleName, types.EventTelemetryKeyBurn, msg.Amount.Denom},
				float32(msg.Amount.Amount.Int64()),
				[]metrics.Label{telemetry.NewLabel(types.EventTelemetryLabelDenom, msg.Amount.Denom)},
			)
		}
	}()

	return &types.MsgBurnResponse{}, nil
}

// Withdraw handles a message to withdraw coins from the marker account.
func (k msgServer) Withdraw(goCtx context.Context, msg *types.MsgWithdrawRequest) (*types.MsgWithdrawResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	// Validate transaction message.
	if err := msg.ValidateBasic(); err != nil {
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	admin := sdk.MustAccAddressFromBech32(msg.Administrator)
	to := sdk.MustAccAddressFromBech32(msg.ToAddress)

	if err := k.Keeper.WithdrawCoins(ctx, admin, to, msg.Denom, msg.Amount); err != nil {
		ctx.Logger().Error("unable to withdraw coins from marker", "err", err)
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	defer func() {
		telemetry.IncrCounterWithLabels(
			[]string{types.ModuleName, types.EventTelemetryKeyWithdraw},
			1,
			[]metrics.Label{
				telemetry.NewLabel(types.EventTelemetryLabelToAddress, msg.ToAddress),
				telemetry.NewLabel(types.EventTelemetryLabelDenom, msg.GetDenom()),
				telemetry.NewLabel(types.EventTelemetryLabelAdministrator, msg.Administrator),
			},
		)
		for _, coin := range msg.Amount {
			if coin.Amount.IsInt64() {
				telemetry.SetGaugeWithLabels(
					[]string{types.ModuleName, types.EventTelemetryKeyWithdraw, msg.Denom},
					float32(coin.Amount.Int64()),
					[]metrics.Label{telemetry.NewLabel(types.EventTelemetryLabelDenom, coin.Denom)},
				)
			}
		}
	}()

	return &types.MsgWithdrawResponse{}, nil
}

// Transfer handles a message to send coins from one account to another (used with restricted coins that are not
//
//	sent using the normal bank send process)
func (k msgServer) Transfer(goCtx context.Context, msg *types.MsgTransferRequest) (*types.MsgTransferResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	// Validate transaction message.
	if err := msg.ValidateBasic(); err != nil {
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	from := sdk.MustAccAddressFromBech32(msg.FromAddress)
	to := sdk.MustAccAddressFromBech32(msg.ToAddress)
	admin := sdk.MustAccAddressFromBech32(msg.Administrator)

	err := k.TransferCoin(ctx, from, to, admin, msg.Amount)
	if err != nil {
		return nil, err
	}

	defer func() {
		telemetry.IncrCounterWithLabels(
			[]string{types.ModuleName, types.EventTelemetryKeyTransfer},
			1,
			[]metrics.Label{
				telemetry.NewLabel(types.EventTelemetryLabelToAddress, msg.ToAddress),
				telemetry.NewLabel(types.EventTelemetryLabelFromAddress, msg.FromAddress),
				telemetry.NewLabel(types.EventTelemetryLabelDenom, msg.Amount.Denom),
				telemetry.NewLabel(types.EventTelemetryLabelAdministrator, msg.Administrator),
			},
		)
		if msg.Amount.Amount.IsInt64() {
			telemetry.SetGaugeWithLabels(
				[]string{types.ModuleName, types.EventTelemetryKeyTransfer, msg.Amount.Denom},
				float32(msg.Amount.Amount.Int64()),
				[]metrics.Label{telemetry.NewLabel(types.EventTelemetryLabelDenom, msg.Amount.Denom)},
			)
		}
	}()

	return &types.MsgTransferResponse{}, nil
}

// IbcTransfer handles a message to ibc send coins from one account to another (used with restricted coins that are not
//
//	sent using the normal ibc send process)
func (k msgServer) IbcTransfer(goCtx context.Context, msg *types.MsgIbcTransferRequest) (*types.MsgIbcTransferResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	// Validate transaction message.
	if err := msg.ValidateBasic(); err != nil {
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	from := sdk.MustAccAddressFromBech32(msg.Transfer.Sender)
	admin := sdk.MustAccAddressFromBech32(msg.Administrator)

	err := k.IbcTransferCoin(ctx, msg.Transfer.SourcePort, msg.Transfer.SourceChannel, msg.Transfer.Token, from, admin, msg.Transfer.Receiver, msg.Transfer.TimeoutHeight, msg.Transfer.TimeoutTimestamp, msg.Transfer.Memo)
	if err != nil {
		return nil, err
	}

	defer func() {
		telemetry.IncrCounterWithLabels(
			[]string{types.ModuleName, types.EventTelemetryKeyIbcTransfer},
			1,
			[]metrics.Label{
				telemetry.NewLabel(types.EventTelemetryLabelToAddress, msg.Transfer.Receiver),
				telemetry.NewLabel(types.EventTelemetryLabelFromAddress, msg.Transfer.Sender),
				telemetry.NewLabel(types.EventTelemetryLabelDenom, msg.Transfer.Token.Denom),
				telemetry.NewLabel(types.EventTelemetryLabelAdministrator, msg.Administrator),
			},
		)
		if msg.Transfer.Token.Amount.IsInt64() {
			telemetry.SetGaugeWithLabels(
				[]string{types.ModuleName, types.EventTelemetryKeyIbcTransfer, msg.Transfer.Token.Denom},
				float32(msg.Transfer.Token.Amount.Int64()),
				[]metrics.Label{telemetry.NewLabel(types.EventTelemetryLabelDenom, msg.Transfer.Token.Denom)},
			)
		}
	}()

	return &types.MsgIbcTransferResponse{}, nil
}

// SetDenomMetadata handles a message setting metadata for a marker with the specified denom.
func (k msgServer) SetDenomMetadata(
	goCtx context.Context,
	msg *types.MsgSetDenomMetadataRequest,
) (*types.MsgSetDenomMetadataResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	// Validate transaction message.
	if err := msg.ValidateBasic(); err != nil {
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	admin := sdk.MustAccAddressFromBech32(msg.Administrator)

	err := k.SetMarkerDenomMetadata(ctx, msg.Metadata, admin)
	if err != nil {
		return nil, err
	}

	return &types.MsgSetDenomMetadataResponse{}, nil
}

// AddFinalizeActivateMarker Handle a message to add a new marker account, finalize it and activate it in one go.
func (k msgServer) AddFinalizeActivateMarker(goCtx context.Context, msg *types.MsgAddFinalizeActivateMarkerRequest) (*types.MsgAddFinalizeActivateMarkerResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	var err error
	// Add marker requests must pass extra validation for denom (in addition to regular coin validation expression)
	if err = k.ValidateUnrestictedDenom(ctx, msg.Amount.Denom); err != nil {
		return nil, err
	}

	// since this is a one shot process should have 1 access list member, to have any value for a marker.
	if len(msg.AccessList) == 0 {
		return nil, sdkerrors.ErrInvalidRequest.Wrap("since this will activate the marker, must have at least one access list defined")
	}

	addr := types.MustGetMarkerAddress(msg.Amount.Denom)
	var manager sdk.AccAddress
	// if manager not supplied set manager from the --from-address
	if msg.Manager != "" {
		manager, err = sdk.AccAddressFromBech32(msg.Manager)
	} else {
		manager, err = sdk.AccAddressFromBech32(msg.FromAddress)
	}
	if err != nil {
		return nil, err
	}

	normalizedReqAttrs, err := k.NormalizeRequiredAttributes(ctx, msg.RequiredAttributes)
	if err != nil {
		return nil, err
	}

	account := authtypes.NewBaseAccount(addr, nil, 0, 0)
	ma := types.NewMarkerAccount(
		account,
		sdk.NewCoin(msg.Amount.Denom, msg.Amount.Amount),
		manager,
		msg.AccessList,
		types.StatusProposed,
		msg.MarkerType,
		msg.SupplyFixed,
		msg.AllowGovernanceControl || k.GetEnableGovernance(ctx),
		msg.AllowForcedTransfer,
		normalizedReqAttrs,
	)

	// Only create a NAV entry if an explicit value is given for a NAV.  If a zero value is desired this can be set explicitly in a followup call.
	// This check prevents a proliferation of incorrect NAV entries being recorded when setting up markers.
	if msg.UsdMills > 0 {
		usdMills := sdkmath.NewIntFromUint64(msg.UsdMills)
		err = k.AddSetNetAssetValues(ctx, ma, []types.NetAssetValue{types.NewNetAssetValue(sdk.NewCoin(types.UsdDenom, usdMills), msg.Volume)}, types.ModuleName)
		if err != nil {
			return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
		}
	}

	if err := k.Keeper.AddFinalizeAndActivateMarker(ctx, ma); err != nil {
		ctx.Logger().Error("unable to add, finalize and activate marker", "err", err)
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	return &types.MsgAddFinalizeActivateMarkerResponse{}, nil
}

// SupplyIncreaseProposal can only be called via gov proposal
func (k msgServer) SupplyIncreaseProposal(goCtx context.Context, msg *types.MsgSupplyIncreaseProposalRequest) (*types.MsgSupplyIncreaseProposalResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	if k.GetAuthority() != msg.Authority {
		return nil, errors.Wrapf(govtypes.ErrInvalidSigner, "expected %s got %s", k.GetAuthority(), msg.Authority)
	}

	err := k.Keeper.HandleSupplyIncreaseProposal(ctx, msg.Amount, msg.TargetAddress)
	if err != nil {
		return nil, err
	}

	return &types.MsgSupplyIncreaseProposalResponse{}, nil
}

// DecreaseIncreaseProposal can only be called via gov proposal
func (k msgServer) SupplyDecreaseProposal(goCtx context.Context, msg *types.MsgSupplyDecreaseProposalRequest) (*types.MsgSupplyDecreaseProposalResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	if k.GetAuthority() != msg.Authority {
		return nil, errors.Wrapf(govtypes.ErrInvalidSigner, "expected %s got %s", k.GetAuthority(), msg.Authority)
	}

	err := k.Keeper.HandleSupplyDecreaseProposal(ctx, msg.Amount)
	if err != nil {
		return nil, err
	}

	return &types.MsgSupplyDecreaseProposalResponse{}, nil
}

// UpdateRequiredAttributes will only succeed if signer has transfer authority
func (k msgServer) UpdateRequiredAttributes(goCtx context.Context, msg *types.MsgUpdateRequiredAttributesRequest) (*types.MsgUpdateRequiredAttributesResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	m, err := k.GetMarkerByDenom(ctx, msg.Denom)
	if err != nil {
		return nil, fmt.Errorf("marker not found for %s: %w", msg.Denom, err)
	}
	if m.GetMarkerType() != types.MarkerType_RestrictedCoin {
		return nil, fmt.Errorf("marker %s is not a restricted marker", msg.Denom)
	}

	caller, err := sdk.AccAddressFromBech32(msg.TransferAuthority)
	if err != nil {
		return nil, err
	}

	switch {
	case msg.TransferAuthority == k.GetAuthority():
		if !m.HasGovernanceEnabled() {
			return nil, fmt.Errorf("%s marker does not allow governance control", msg.Denom)
		}
	case !m.AddressHasAccess(caller, types.Access_Transfer):
		return nil, fmt.Errorf("caller does not have authority to update required attributes %s", msg.TransferAuthority)
	}

	removeList, err := k.NormalizeRequiredAttributes(ctx, msg.RemoveRequiredAttributes)
	if err != nil {
		return nil, err
	}
	addList, err := k.NormalizeRequiredAttributes(ctx, msg.AddRequiredAttributes)
	if err != nil {
		return nil, err
	}

	reqAttrs, err := types.RemoveFromRequiredAttributes(m.GetRequiredAttributes(), removeList)
	if err != nil {
		return nil, err
	}
	reqAttrs, err = types.AddToRequiredAttributes(reqAttrs, addList)
	if err != nil {
		return nil, err
	}

	m.SetRequiredAttributes(reqAttrs)
	k.SetMarker(ctx, m)

	return &types.MsgUpdateRequiredAttributesResponse{}, nil
}

// UpdateForcedTransfer updates the allow_forced_transfer field of a marker via governance proposal.
func (k msgServer) UpdateForcedTransfer(goCtx context.Context, msg *types.MsgUpdateForcedTransferRequest) (*types.MsgUpdateForcedTransferResponse, error) {
	if msg.Authority != k.GetAuthority() {
		return nil, errors.Wrapf(govtypes.ErrInvalidSigner, "expected %s got %s", k.GetAuthority(), msg.Authority)
	}

	ctx := sdk.UnwrapSDKContext(goCtx)
	marker, err := k.GetMarkerByDenom(ctx, msg.Denom)
	if err != nil {
		return nil, fmt.Errorf("could not get marker for %s: %w", msg.Denom, err)
	}

	if marker.GetMarkerType() != types.MarkerType_RestrictedCoin {
		return nil, fmt.Errorf("cannot update forced transfer on unrestricted marker %s", msg.Denom)
	}

	if !marker.HasGovernanceEnabled() {
		return nil, fmt.Errorf("%s marker does not allow governance control", msg.Denom)
	}

	if marker.AllowsForcedTransfer() == msg.AllowForcedTransfer {
		return nil, fmt.Errorf("marker %s already has allow_forced_transfer = %t", msg.Denom, msg.AllowForcedTransfer)
	}

	marker.SetAllowForcedTransfer(msg.AllowForcedTransfer)
	k.SetMarker(ctx, marker)

	return &types.MsgUpdateForcedTransferResponse{}, nil
}

// SetAccountData sets the accountdata for a denom. Signer must have deposit authority.
func (k msgServer) SetAccountData(goCtx context.Context, msg *types.MsgSetAccountDataRequest) (*types.MsgSetAccountDataResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	marker, err := k.GetMarkerByDenom(ctx, msg.Denom)
	if err != nil {
		return nil, fmt.Errorf("could not get %s marker: %w", msg.Denom, err)
	}

	if msg.Signer == k.GetAuthority() {
		if !marker.HasGovernanceEnabled() {
			return nil, fmt.Errorf("%s marker does not allow governance control", msg.Denom)
		}
	} else {
		if err = marker.ValidateHasAccess(msg.Signer, types.Access_Deposit); err != nil {
			return nil, err
		}
	}

	err = k.attrKeeper.SetAccountData(ctx, marker.GetAddress().String(), msg.Value)
	if err != nil {
		return nil, fmt.Errorf("error setting %s account data: %w", msg.Denom, err)
	}

	return &types.MsgSetAccountDataResponse{}, nil
}

// UpdateSendDenyList updates the deny send list for restricted marker. Signer must be admin or gov proposal.
func (k msgServer) UpdateSendDenyList(goCtx context.Context, msg *types.MsgUpdateSendDenyListRequest) (*types.MsgUpdateSendDenyListResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	marker, err := k.GetMarkerByDenom(ctx, msg.Denom)
	if err != nil {
		return nil, fmt.Errorf("marker not found for %s: %w", msg.Denom, err)
	}

	if marker.GetMarkerType() != types.MarkerType_RestrictedCoin {
		return nil, fmt.Errorf("marker %s is not a restricted marker", msg.Denom)
	}

	if msg.Authority == k.GetAuthority() {
		if !marker.HasGovernanceEnabled() {
			return nil, fmt.Errorf("%s marker does not allow governance control", msg.Denom)
		}
	} else if err = marker.ValidateHasAccess(msg.Authority, types.Access_Transfer); err != nil {
		return nil, err
	}

	markerAddr := marker.GetAddress()
	for _, addr := range msg.RemoveDeniedAddresses {
		denyAddr, err := sdk.AccAddressFromBech32(addr)
		if err != nil {
			return nil, err
		}
		if !k.IsSendDeny(ctx, markerAddr, denyAddr) {
			return nil, fmt.Errorf("%s is not on deny list cannot remove address", addr)
		}
		k.RemoveSendDeny(ctx, markerAddr, denyAddr)
	}

	for _, addr := range msg.AddDeniedAddresses {
		denyAddr, err := sdk.AccAddressFromBech32(addr)
		if err != nil {
			return nil, err
		}
		if k.IsSendDeny(ctx, markerAddr, denyAddr) {
			return nil, fmt.Errorf("%s is already on deny list cannot add address", addr)
		}
		k.AddSendDeny(ctx, markerAddr, denyAddr)
	}

	return &types.MsgUpdateSendDenyListResponse{}, nil
}

// AddNetAssetValues adds net asset values to a marker
func (k msgServer) AddNetAssetValues(goCtx context.Context, msg *types.MsgAddNetAssetValuesRequest) (*types.MsgAddNetAssetValuesResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	marker, err := k.GetMarkerByDenom(ctx, msg.Denom)
	if err != nil {
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	isGovProp := marker.HasGovernanceEnabled() && msg.Administrator == k.GetAuthority()

	if !isGovProp {
		admin := sdk.MustAccAddressFromBech32(msg.Administrator)
		hasGrants := types.GrantsForAddress(admin, marker.GetAccessList()...).GetAccessList()
		if len(hasGrants) == 0 {
			return nil, fmt.Errorf("signer %v does not have permission to add net asset value for %q", msg.Administrator, marker.GetDenom())
		}
	}

	err = k.AddSetNetAssetValues(ctx, marker, msg.NetAssetValues, msg.Administrator)
	if err != nil {
		return nil, sdkerrors.ErrInvalidRequest.Wrap(err.Error())
	}

	return &types.MsgAddNetAssetValuesResponse{}, nil
}

// SetAdministratorProposal can only be called via gov proposal
func (k msgServer) SetAdministratorProposal(goCtx context.Context, msg *types.MsgSetAdministratorProposalRequest) (*types.MsgSetAdministratorProposalResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	if k.GetAuthority() != msg.Authority {
		return nil, errors.Wrapf(govtypes.ErrInvalidSigner, "expected %s got %s", k.GetAuthority(), msg.Authority)
	}

	err := k.Keeper.HandleSetAdministratorProposal(ctx, msg.Denom, msg.Access)
	if err != nil {
		return nil, err
	}

	return &types.MsgSetAdministratorProposalResponse{}, nil
}

// RemoveAdministratorProposal can only be called via gov proposal
func (k msgServer) RemoveAdministratorProposal(goCtx context.Context, msg *types.MsgRemoveAdministratorProposalRequest) (*types.MsgRemoveAdministratorProposalResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	if k.GetAuthority() != msg.Authority {
		return nil, errors.Wrapf(govtypes.ErrInvalidSigner, "expected %s got %s", k.GetAuthority(), msg.Authority)
	}

	err := k.Keeper.HandleRemoveAdministratorProposal(ctx, msg.Denom, msg.RemovedAddress)
	if err != nil {
		return nil, err
	}

	return &types.MsgRemoveAdministratorProposalResponse{}, nil
}

// ChangeStatusProposal can only be called via gov proposal
func (k msgServer) ChangeStatusProposal(goCtx context.Context, msg *types.MsgChangeStatusProposalRequest) (*types.MsgChangeStatusProposalResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	if k.GetAuthority() != msg.Authority {
		return nil, errors.Wrapf(govtypes.ErrInvalidSigner, "expected %s got %s", k.GetAuthority(), msg.Authority)
	}

	err := k.Keeper.HandleChangeStatusProposal(ctx, msg.Denom, msg.NewStatus)
	if err != nil {
		return nil, err
	}

	return &types.MsgChangeStatusProposalResponse{}, nil
}

// WithdrawEscrowProposal can only be called via gov proposal
func (k msgServer) WithdrawEscrowProposal(goCtx context.Context, msg *types.MsgWithdrawEscrowProposalRequest) (*types.MsgWithdrawEscrowProposalResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	if k.GetAuthority() != msg.Authority {
		return nil, errors.Wrapf(govtypes.ErrInvalidSigner, "expected %s got %s", k.GetAuthority(), msg.Authority)
	}

	err := k.Keeper.HandleWithdrawEscrowProposal(ctx, msg.Denom, msg.TargetAddress, msg.Amount)
	if err != nil {
		return nil, err
	}

	return &types.MsgWithdrawEscrowProposalResponse{}, nil
}

// SetDenomMetaDataProposal can only be called via gov proposal
func (k msgServer) SetDenomMetadataProposal(goCtx context.Context, msg *types.MsgSetDenomMetadataProposalRequest) (*types.MsgSetDenomMetadataProposalResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	if k.GetAuthority() != msg.Authority {
		return nil, errors.Wrapf(govtypes.ErrInvalidSigner, "expected %s got %s", k.GetAuthority(), msg.Authority)
	}

	err := k.Keeper.HandleSetDenomMetadataProposal(ctx, msg.Metadata)
	if err != nil {
		return nil, err
	}

	return &types.MsgSetDenomMetadataProposalResponse{}, nil
}

// UpdateParams is a governance proposal endpoint for updating the marker module's params.
func (k msgServer) UpdateParams(goCtx context.Context, msg *types.MsgUpdateParamsRequest) (*types.MsgUpdateParamsResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	if err := k.ValidateAuthority(msg.Authority); err != nil {
		return nil, err
	}

	k.SetParams(ctx, msg.Params)
	if err := ctx.EventManager().EmitTypedEvent(types.NewEventMarkerParamsUpdated(msg.Params.EnableGovernance, msg.Params.GetUnrestrictedDenomRegex(), msg.Params.MaxSupply)); err != nil {
		return nil, err
	}

	return &types.MsgUpdateParamsResponse{}, nil
}
