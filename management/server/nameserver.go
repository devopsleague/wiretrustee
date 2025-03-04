package server

import (
	"errors"
	"regexp"
	"unicode/utf8"

	"github.com/miekg/dns"
	"github.com/rs/xid"
	log "github.com/sirupsen/logrus"

	nbdns "github.com/netbirdio/netbird/dns"
	"github.com/netbirdio/netbird/management/server/activity"
	"github.com/netbirdio/netbird/management/server/status"
)

const domainPattern = `^(?i)[a-z0-9]+([\-\.]{1}[a-z0-9]+)*\.[a-z]{2,}$`

// GetNameServerGroup gets a nameserver group object from account and nameserver group IDs
func (am *DefaultAccountManager) GetNameServerGroup(accountID, nsGroupID string) (*nbdns.NameServerGroup, error) {

	unlock := am.Store.AcquireAccountLock(accountID)
	defer unlock()

	account, err := am.Store.GetAccount(accountID)
	if err != nil {
		return nil, err
	}

	nsGroup, found := account.NameServerGroups[nsGroupID]
	if found {
		return nsGroup.Copy(), nil
	}

	return nil, status.Errorf(status.NotFound, "nameserver group with ID %s not found", nsGroupID)
}

// CreateNameServerGroup creates and saves a new nameserver group
func (am *DefaultAccountManager) CreateNameServerGroup(accountID string, name, description string, nameServerList []nbdns.NameServer, groups []string, primary bool, domains []string, enabled bool, userID string) (*nbdns.NameServerGroup, error) {

	unlock := am.Store.AcquireAccountLock(accountID)
	defer unlock()

	account, err := am.Store.GetAccount(accountID)
	if err != nil {
		return nil, err
	}

	newNSGroup := &nbdns.NameServerGroup{
		ID:          xid.New().String(),
		Name:        name,
		Description: description,
		NameServers: nameServerList,
		Groups:      groups,
		Enabled:     enabled,
		Primary:     primary,
		Domains:     domains,
	}

	err = validateNameServerGroup(false, newNSGroup, account)
	if err != nil {
		return nil, err
	}

	if account.NameServerGroups == nil {
		account.NameServerGroups = make(map[string]*nbdns.NameServerGroup)
	}

	account.NameServerGroups[newNSGroup.ID] = newNSGroup

	account.Network.IncSerial()
	err = am.Store.SaveAccount(account)
	if err != nil {
		return nil, err
	}

	err = am.updateAccountPeers(account)
	if err != nil {
		log.Error(err)
		return newNSGroup.Copy(), status.Errorf(status.Internal, "failed to update peers after create nameserver %s", name)
	}

	am.storeEvent(userID, newNSGroup.ID, accountID, activity.NameserverGroupCreated, newNSGroup.EventMeta())

	return newNSGroup.Copy(), nil
}

// SaveNameServerGroup saves nameserver group
func (am *DefaultAccountManager) SaveNameServerGroup(accountID, userID string, nsGroupToSave *nbdns.NameServerGroup) error {

	unlock := am.Store.AcquireAccountLock(accountID)
	defer unlock()

	if nsGroupToSave == nil {
		return status.Errorf(status.InvalidArgument, "nameserver group provided is nil")
	}

	account, err := am.Store.GetAccount(accountID)
	if err != nil {
		return err
	}

	err = validateNameServerGroup(true, nsGroupToSave, account)
	if err != nil {
		return err
	}

	account.NameServerGroups[nsGroupToSave.ID] = nsGroupToSave

	account.Network.IncSerial()
	err = am.Store.SaveAccount(account)
	if err != nil {
		return err
	}

	err = am.updateAccountPeers(account)
	if err != nil {
		log.Error(err)
		return status.Errorf(status.Internal, "failed to update peers after update nameserver %s", nsGroupToSave.Name)
	}

	am.storeEvent(userID, nsGroupToSave.ID, accountID, activity.NameserverGroupUpdated, nsGroupToSave.EventMeta())

	return nil
}

// DeleteNameServerGroup deletes nameserver group with nsGroupID
func (am *DefaultAccountManager) DeleteNameServerGroup(accountID, nsGroupID, userID string) error {

	unlock := am.Store.AcquireAccountLock(accountID)
	defer unlock()

	account, err := am.Store.GetAccount(accountID)
	if err != nil {
		return err
	}

	nsGroup := account.NameServerGroups[nsGroupID]
	if nsGroup == nil {
		return status.Errorf(status.NotFound, "nameserver group %s wasn't found", nsGroupID)
	}
	delete(account.NameServerGroups, nsGroupID)

	account.Network.IncSerial()
	err = am.Store.SaveAccount(account)
	if err != nil {
		return err
	}

	err = am.updateAccountPeers(account)
	if err != nil {
		return status.Errorf(status.Internal, "failed to update peers after deleting nameserver %s", nsGroupID)
	}

	am.storeEvent(userID, nsGroup.ID, accountID, activity.NameserverGroupDeleted, nsGroup.EventMeta())

	return nil
}

// ListNameServerGroups returns a list of nameserver groups from account
func (am *DefaultAccountManager) ListNameServerGroups(accountID string) ([]*nbdns.NameServerGroup, error) {

	unlock := am.Store.AcquireAccountLock(accountID)
	defer unlock()

	account, err := am.Store.GetAccount(accountID)
	if err != nil {
		return nil, err
	}

	nsGroups := make([]*nbdns.NameServerGroup, 0, len(account.NameServerGroups))
	for _, item := range account.NameServerGroups {
		nsGroups = append(nsGroups, item.Copy())
	}

	return nsGroups, nil
}

func validateNameServerGroup(existingGroup bool, nameserverGroup *nbdns.NameServerGroup, account *Account) error {
	nsGroupID := ""
	if existingGroup {
		nsGroupID = nameserverGroup.ID
		_, found := account.NameServerGroups[nsGroupID]
		if !found {
			return status.Errorf(status.NotFound, "nameserver group with ID %s was not found", nsGroupID)
		}
	}

	err := validateDomainInput(nameserverGroup.Primary, nameserverGroup.Domains)
	if err != nil {
		return err
	}

	err = validateNSGroupName(nameserverGroup.Name, nsGroupID, account.NameServerGroups)
	if err != nil {
		return err
	}

	err = validateNSList(nameserverGroup.NameServers)
	if err != nil {
		return err
	}

	err = validateGroups(nameserverGroup.Groups, account.Groups)
	if err != nil {
		return err
	}

	return nil
}

func validateDomainInput(primary bool, domains []string) error {
	if !primary && len(domains) == 0 {
		return status.Errorf(status.InvalidArgument, "nameserver group primary status is false and domains are empty,"+
			" it should be primary or have at least one domain")
	}
	if primary && len(domains) != 0 {
		return status.Errorf(status.InvalidArgument, "nameserver group primary status is true and domains are not empty,"+
			" you should set either primary or domain")
	}
	for _, domain := range domains {
		if err := validateDomain(domain); err != nil {
			return status.Errorf(status.InvalidArgument, "nameserver group got an invalid domain: %s %q", domain, err)
		}
	}
	return nil
}

func validateNSGroupName(name, nsGroupID string, nsGroupMap map[string]*nbdns.NameServerGroup) error {
	if utf8.RuneCountInString(name) > nbdns.MaxGroupNameChar || name == "" {
		return status.Errorf(status.InvalidArgument, "nameserver group name should be between 1 and %d", nbdns.MaxGroupNameChar)
	}

	for _, nsGroup := range nsGroupMap {
		if name == nsGroup.Name && nsGroup.ID != nsGroupID {
			return status.Errorf(status.InvalidArgument, "a nameserver group with name %s already exist", name)
		}
	}

	return nil
}

func validateNSList(list []nbdns.NameServer) error {
	nsListLenght := len(list)
	if nsListLenght == 0 || nsListLenght > 2 {
		return status.Errorf(status.InvalidArgument, "the list of nameservers should be 1 or 2, got %d", len(list))
	}
	return nil
}

func validateGroups(list []string, groups map[string]*Group) error {
	if len(list) == 0 {
		return status.Errorf(status.InvalidArgument, "the list of group IDs should not be empty")
	}

	for _, id := range list {
		if id == "" {
			return status.Errorf(status.InvalidArgument, "group ID should not be empty string")
		}
		found := false
		for groupID := range groups {
			if id == groupID {
				found = true
				break
			}
		}
		if !found {
			return status.Errorf(status.InvalidArgument, "group id %s not found", id)
		}
	}

	return nil
}

func validateDomain(domain string) error {
	domainMatcher := regexp.MustCompile(domainPattern)
	if !domainMatcher.MatchString(domain) {
		return errors.New("domain should consists of only letters, numbers, and hyphens with no leading, trailing hyphens, or spaces")
	}

	labels, valid := dns.IsDomainName(domain)
	if !valid {
		return errors.New("invalid domain name")
	}

	if labels < 2 {
		return errors.New("domain should consists of a minimum of two labels")
	}

	return nil
}
