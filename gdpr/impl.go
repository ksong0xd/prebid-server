package gdpr

import (
	"context"
	"errors"
	"fmt"

	"github.com/prebid/go-gdpr/api"
	"github.com/prebid/go-gdpr/consentconstants"
	tcf2ConsentConstants "github.com/prebid/go-gdpr/consentconstants/tcf2"
	"github.com/prebid/go-gdpr/vendorconsent"
	tcf2 "github.com/prebid/go-gdpr/vendorconsent/tcf2"
	"github.com/prebid/prebid-server/openrtb_ext"
)

type permissionsImpl struct {
	fetchVendorList       VendorListFetcher
	gdprDefaultValue      string
	hostVendorID          int
	nonStandardPublishers map[string]struct{}
	cfg                   TCF2ConfigReader
	vendorIDs             map[openrtb_ext.BidderName]uint16
	consent               string
	gdprSignal            Signal
	publisherID           string
	aliasGVLIDs           map[string]uint16
}

func (p *permissionsImpl) HostCookiesAllowed(ctx context.Context) (bool, error) {
	if p.gdprSignal != SignalYes {
		return true, nil
	}

	return p.allowSync(ctx, uint16(p.hostVendorID), false)
}

func (p *permissionsImpl) BidderSyncAllowed(ctx context.Context, bidder openrtb_ext.BidderName) (bool, error) {
	if p.gdprSignal != SignalYes {
		return true, nil
	}

	id, ok := p.vendorIDs[bidder]
	if ok {
		vendorException := p.cfg.PurposeVendorException(consentconstants.Purpose(1), bidder)
		return p.allowSync(ctx, id, vendorException)
	}

	return false, nil
}

func (p *permissionsImpl) AuctionActivitiesAllowed(ctx context.Context, bidderCoreName openrtb_ext.BidderName, bidder openrtb_ext.BidderName) (permissions AuctionPermissions, err error) {
	if _, ok := p.nonStandardPublishers[p.publisherID]; ok {
		return AllowAll, nil
	}

	if p.gdprSignal != SignalYes {
		return AllowAll, nil
	}

	if p.consent == "" {
		return p.defaultPermissions(), nil
	}

	weakVendorEnforcement := p.cfg.BasicEnforcementVendor(bidder)

	if id, ok := p.resolveVendorId(bidderCoreName, bidder); ok {
		return p.allowActivities(ctx, id, bidderCoreName, weakVendorEnforcement)
	} else if weakVendorEnforcement {
		return p.allowActivities(ctx, 0, bidderCoreName, weakVendorEnforcement)
	}

	return DenyAll, nil
}

// defaultPermissions returns a permissions object that denies passing user IDs while
// allowing passing geo information and sending bid requests based on whether purpose 2
// and feature one are enforced respectively
// if the consent string is empty or malformed we should use the default permissions
func (p *permissionsImpl) defaultPermissions() AuctionPermissions {
	perms := AuctionPermissions{}

	if !p.cfg.PurposeEnforced(consentconstants.Purpose(2)) {
		perms.AllowBidRequest = true
	}
	if !p.cfg.FeatureOneEnforced() {
		perms.PassGeo = true
	}
	return perms
}

func (p *permissionsImpl) resolveVendorId(bidderCoreName openrtb_ext.BidderName, bidder openrtb_ext.BidderName) (id uint16, ok bool) {
	if id, ok = p.aliasGVLIDs[string(bidder)]; ok {
		return id, ok
	}

	id, ok = p.vendorIDs[bidderCoreName]

	return id, ok
}

func (p *permissionsImpl) allowSync(ctx context.Context, vendorID uint16, vendorException bool) (bool, error) {
	if p.consent == "" {
		return false, nil
	}

	parsedConsent, vendor, err := p.parseVendor(ctx, vendorID, p.consent)
	if err != nil {
		return false, err
	}

	if vendor == nil {
		return false, nil
	}

	if !p.cfg.PurposeEnforced(consentconstants.Purpose(1)) {
		return true, nil
	}
	consentMeta, ok := parsedConsent.(tcf2.ConsentMetadata)
	if !ok {
		err := errors.New("Unable to access TCF2 parsed consent")
		return false, err
	}

	if p.cfg.PurposeOneTreatmentEnabled() && consentMeta.PurposeOneTreatment() {
		return p.cfg.PurposeOneTreatmentAccessAllowed(), nil
	}

	enforceVendors := p.cfg.PurposeEnforcingVendors(tcf2ConsentConstants.InfoStorageAccess)
	return p.checkPurpose(consentMeta, vendor, vendorID, tcf2ConsentConstants.InfoStorageAccess, enforceVendors, vendorException, false), nil
}

func (p *permissionsImpl) allowActivities(ctx context.Context, vendorID uint16, bidder openrtb_ext.BidderName, weakVendorEnforcement bool) (AuctionPermissions, error) {
	parsedConsent, vendor, err := p.parseVendor(ctx, vendorID, p.consent)
	if err != nil {
		return p.defaultPermissions(), err
	}

	// vendor will be nil if not a valid TCF2 consent string
	if vendor == nil {
		if weakVendorEnforcement && parsedConsent.Version() == 2 {
			vendor = vendorTrue{}
		} else {
			return p.defaultPermissions(), nil
		}
	}

	if !p.cfg.IsEnabled() {
		return AllowBidRequestOnly, nil
	}

	consentMeta, ok := parsedConsent.(tcf2.ConsentMetadata)
	if !ok {
		err = fmt.Errorf("Unable to access TCF2 parsed consent")
		return p.defaultPermissions(), err
	}

	permissions := AuctionPermissions{}
	if p.cfg.FeatureOneEnforced() {
		vendorException := p.cfg.FeatureOneVendorException(bidder)
		permissions.PassGeo = vendorException || (consentMeta.SpecialFeatureOptIn(1) && (vendor.SpecialFeature(1) || weakVendorEnforcement))
	} else {
		permissions.PassGeo = true
	}
	if p.cfg.PurposeEnforced(consentconstants.Purpose(2)) {
		enforceVendors := p.cfg.PurposeEnforcingVendors(consentconstants.Purpose(2))
		vendorException := p.cfg.PurposeVendorException(consentconstants.Purpose(2), bidder)
		permissions.AllowBidRequest = p.checkPurpose(consentMeta, vendor, vendorID, consentconstants.Purpose(2), enforceVendors, vendorException, weakVendorEnforcement)
	} else {
		permissions.AllowBidRequest = true
	}
	for i := 2; i <= 10; i++ {
		enforceVendors := p.cfg.PurposeEnforcingVendors(consentconstants.Purpose(i))
		vendorException := p.cfg.PurposeVendorException(consentconstants.Purpose(i), bidder)
		if p.checkPurpose(consentMeta, vendor, vendorID, consentconstants.Purpose(i), enforceVendors, vendorException, weakVendorEnforcement) {
			permissions.PassID = true
			break
		}
	}

	return permissions, nil
}

const pubRestrictNotAllowed = 0
const pubRestrictRequireConsent = 1
const pubRestrictRequireLegitInterest = 2

func (p *permissionsImpl) checkPurpose(consent tcf2.ConsentMetadata, vendor api.Vendor, vendorID uint16, purpose consentconstants.Purpose, enforceVendors, vendorException, weakVendorEnforcement bool) bool {
	if consent.CheckPubRestriction(uint8(purpose), pubRestrictNotAllowed, vendorID) {
		return false
	}

	if vendorException {
		return true
	}

	purposeAllowed := p.consentEstablished(consent, vendor, vendorID, purpose, enforceVendors, weakVendorEnforcement)
	legitInterest := p.legitInterestEstablished(consent, vendor, vendorID, purpose, enforceVendors, weakVendorEnforcement)

	if consent.CheckPubRestriction(uint8(purpose), pubRestrictRequireConsent, vendorID) {
		return purposeAllowed
	}
	if consent.CheckPubRestriction(uint8(purpose), pubRestrictRequireLegitInterest, vendorID) {
		// Need LITransparency here
		return legitInterest
	}

	return purposeAllowed || legitInterest
}

func (p *permissionsImpl) consentEstablished(consent tcf2.ConsentMetadata, vendor api.Vendor, vendorID uint16, purpose consentconstants.Purpose, enforceVendors, weakVendorEnforcement bool) bool {
	if !consent.PurposeAllowed(purpose) {
		return false
	}
	if weakVendorEnforcement {
		return true
	}
	if !enforceVendors {
		return true
	}
	if vendor.Purpose(purpose) && consent.VendorConsent(vendorID) {
		return true
	}
	return false
}

func (p *permissionsImpl) legitInterestEstablished(consent tcf2.ConsentMetadata, vendor api.Vendor, vendorID uint16, purpose consentconstants.Purpose, enforceVendors, weakVendorEnforcement bool) bool {
	if !consent.PurposeLITransparency(purpose) {
		return false
	}
	if weakVendorEnforcement {
		return true
	}
	if !enforceVendors {
		return true
	}
	if vendor.LegitimateInterest(purpose) && consent.VendorLegitInterest(vendorID) {
		return true
	}
	return false
}

func (p *permissionsImpl) parseVendor(ctx context.Context, vendorID uint16, consent string) (parsedConsent api.VendorConsents, vendor api.Vendor, err error) {
	parsedConsent, err = vendorconsent.ParseString(consent)
	if err != nil {
		err = &ErrorMalformedConsent{
			Consent: consent,
			Cause:   err,
		}
		return
	}

	version := parsedConsent.Version()
	if version != 2 {
		return
	}

	vendorList, err := p.fetchVendorList(ctx, parsedConsent.VendorListVersion())
	if err != nil {
		return
	}

	vendor = vendorList.Vendor(vendorID)
	return
}

// AllowHostCookies represents a GDPR permissions policy with host cookie syncing always allowed
type AllowHostCookies struct {
	*permissionsImpl
}

// HostCookiesAllowed always returns true
func (p *AllowHostCookies) HostCookiesAllowed(ctx context.Context) (bool, error) {
	return true, nil
}

// Exporting to allow for easy test setups
type AlwaysAllow struct{}

func (a AlwaysAllow) HostCookiesAllowed(ctx context.Context) (bool, error) {
	return true, nil
}

func (a AlwaysAllow) BidderSyncAllowed(ctx context.Context, bidder openrtb_ext.BidderName) (bool, error) {
	return true, nil
}

func (a AlwaysAllow) AuctionActivitiesAllowed(ctx context.Context, bidderCoreName openrtb_ext.BidderName, bidder openrtb_ext.BidderName) (permissions AuctionPermissions, err error) {
	return AllowAll, nil
}

// vendorTrue claims everything.
type vendorTrue struct{}

func (v vendorTrue) Purpose(purposeID consentconstants.Purpose) bool {
	return true
}
func (v vendorTrue) PurposeStrict(purposeID consentconstants.Purpose) bool {
	return true
}
func (v vendorTrue) LegitimateInterest(purposeID consentconstants.Purpose) bool {
	return true
}
func (v vendorTrue) LegitimateInterestStrict(purposeID consentconstants.Purpose) bool {
	return true
}
func (v vendorTrue) SpecialFeature(featureID consentconstants.SpecialFeature) (hasSpecialFeature bool) {
	return true
}
func (v vendorTrue) SpecialPurpose(purposeID consentconstants.Purpose) (hasSpecialPurpose bool) {
	return true
}
