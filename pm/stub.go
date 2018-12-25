package pm

import (
	"fmt"
	"math/big"

	ethcommon "github.com/ethereum/go-ethereum/common"
)

type stubTicketStore struct {
	tickets        map[string][]*Ticket
	sigs           map[string][][]byte
	recipientRands map[string][]*big.Int
}

func newStubTicketStore() *stubTicketStore {
	return &stubTicketStore{
		tickets:        make(map[string][]*Ticket),
		sigs:           make(map[string][][]byte),
		recipientRands: make(map[string][]*big.Int),
	}
}

func (ts *stubTicketStore) Store(sessionID string, ticket *Ticket, sig []byte, recipientRand *big.Int) error {
	ts.tickets[sessionID] = append(ts.tickets[sessionID], ticket)
	ts.sigs[sessionID] = append(ts.sigs[sessionID], sig)
	ts.recipientRands[sessionID] = append(ts.recipientRands[sessionID], recipientRand)

	return nil
}

func (ts *stubTicketStore) Load(sessionID string) ([]*Ticket, [][]byte, []*big.Int, error) {
	return ts.tickets[sessionID], ts.sigs[sessionID], ts.recipientRands[sessionID], nil
}

type stubSigVerifier struct {
	verifyResult bool
}

func (sv *stubSigVerifier) SetVerifyResult(verifyResult bool) {
	sv.verifyResult = verifyResult
}

func (sv *stubSigVerifier) Verify(addr ethcommon.Address, msg, sig []byte) bool {
	return sv.verifyResult
}

type stubBroker struct {
	deposits        map[ethcommon.Address]*big.Int
	penaltyEscrows  map[ethcommon.Address]*big.Int
	usedTickets     map[ethcommon.Hash]bool
	approvedSigners map[ethcommon.Address]bool
}

func newStubBroker() *stubBroker {
	return &stubBroker{
		deposits:        make(map[ethcommon.Address]*big.Int),
		penaltyEscrows:  make(map[ethcommon.Address]*big.Int),
		usedTickets:     make(map[ethcommon.Hash]bool),
		approvedSigners: make(map[ethcommon.Address]bool),
	}
}

func (b *stubBroker) FundAndApproveSigners(depositAmount *big.Int, penaltyEscrowAmount *big.Int, signers []ethcommon.Address) error {
	return nil
}

func (b *stubBroker) FundDeposit(amount *big.Int) error {
	return nil
}

func (b *stubBroker) FundPenaltyEscrow(amount *big.Int) error {
	return nil
}

func (b *stubBroker) ApproveSigners(signers []ethcommon.Address) error {
	for i := 0; i < len(signers); i++ {
		b.approvedSigners[signers[i]] = true
	}

	return nil
}

func (b *stubBroker) RequestSignersRevocation(signers []ethcommon.Address) error {
	return nil
}

func (b *stubBroker) Unlock() error {
	return nil
}

func (b *stubBroker) CancelUnlock() error {
	return nil
}

func (b *stubBroker) Withdraw() error {
	return nil
}

func (b *stubBroker) RedeemWinningTicket(ticket *Ticket, sig []byte, recipientRand *big.Int) error {
	b.usedTickets[ticket.Hash()] = true

	return nil
}

func (b *stubBroker) IsUsedTicket(ticket *Ticket) (bool, error) {
	return b.usedTickets[ticket.Hash()], nil
}

func (b *stubBroker) IsApprovedSigner(sender ethcommon.Address, signer ethcommon.Address) (bool, error) {
	return b.approvedSigners[signer], nil
}

func (b *stubBroker) SetDeposit(addr ethcommon.Address, amount *big.Int) {
	b.deposits[addr] = amount
}

func (b *stubBroker) GetDeposit(addr ethcommon.Address) (*big.Int, error) {
	deposit, ok := b.deposits[addr]
	if !ok {
		return nil, fmt.Errorf("no deposit for 0x%x", addr)
	}

	return deposit, nil
}

func (b *stubBroker) SetPenaltyEscrow(addr ethcommon.Address, amount *big.Int) {
	b.penaltyEscrows[addr] = amount
}

func (b *stubBroker) GetPenaltyEscrow(addr ethcommon.Address) (*big.Int, error) {
	penaltyEscrow, ok := b.penaltyEscrows[addr]
	if !ok {
		return nil, fmt.Errorf("no penalty escrow for 0x%x", addr)
	}

	return penaltyEscrow, nil
}

type stubValidator struct {
	isValidTicket   bool
	isWinningTicket bool
}

func (v *stubValidator) SetIsValidTicket(isValidTicket bool) {
	v.isValidTicket = isValidTicket
}

func (v *stubValidator) SetIsWinningTicket(isWinningTicket bool) {
	v.isWinningTicket = isWinningTicket
}

func (v *stubValidator) ValidateTicket(ticket *Ticket, sig []byte, recipientRand *big.Int) error {
	if !v.isValidTicket {
		return fmt.Errorf("invalid ticket")
	}

	return nil
}

func (v *stubValidator) IsWinningTicket(ticket *Ticket, sig []byte, recipientRand *big.Int) bool {
	return v.isWinningTicket
}
