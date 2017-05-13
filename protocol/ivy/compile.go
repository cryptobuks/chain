package ivy

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"

	chainjson "chain/encoding/json"
	"chain/errors"
	"chain/protocol/vm"
)

type (
	CompileResult struct {
		Name    string             `json:"name"`
		Program chainjson.HexBytes `json:"program"`
		Value   string             `json:"value"`
		Params  []ContractParam    `json:"params"`
		Clauses []ClauseInfo       `json:"clause_info"`
	}

	ContractParam struct {
		Name string `json:"name"`
		Typ  string `json:"type"`
	}

	ClauseInfo struct {
		Name   string      `json:"name"`
		Args   []ClauseArg `json:"args"`
		Values []ValueInfo `json:"value_info"`

		// Mintimes is the stringified form of "x" for any "verify after(x)" in the clause
		Mintimes []string `json:"mintimes"`

		// Maxtimes is the stringified form of "x" for any "verify before(x)" in the clause
		Maxtimes []string `json:"maxtimes"`

		// Records each call to a hash function and the type of the
		// argument passed in
		HashCalls []hashCall `json:"hash_calls"`
	}

	ClauseArg struct {
		Name string `json:"name"`
		Typ  string `json:"type"`
	}

	ValueInfo struct {
		Name    string `json:"name"`
		Program string `json:"program,omitempty"`
		Asset   string `json:"asset,omitempty"`
		Amount  string `json:"amount,omitempty"`
	}
)

type ContractArg struct {
	B *bool               `json:"boolean,omitempty"`
	I *int64              `json:"integer,omitempty"`
	S *chainjson.HexBytes `json:"string,omitempty"`
}

// Compile parses an Ivy contract from the supplied reader and
// produces the compiled bytecode and other analysis.
func Compile(r io.Reader, args []ContractArg) (CompileResult, error) {
	inp, err := ioutil.ReadAll(r)
	if err != nil {
		return CompileResult{}, errors.Wrap(err, "reading input")
	}
	c, err := parse(inp)
	if err != nil {
		return CompileResult{}, errors.Wrap(err, "parse error")
	}
	prog, err := compileContract(c, args)
	if err != nil {
		return CompileResult{}, errors.Wrap(err, "compiling contract")
	}
	result := CompileResult{
		Name:    c.name,
		Program: prog,
		Params:  []ContractParam{},
		Value:   c.value,
	}
	for _, param := range c.params {
		result.Params = append(result.Params, ContractParam{Name: param.name, Typ: string(param.bestType())})
	}

	for _, clause := range c.clauses {
		info := ClauseInfo{
			Name:      clause.name,
			Args:      []ClauseArg{},
			Mintimes:  clause.mintimes,
			Maxtimes:  clause.maxtimes,
			HashCalls: clause.hashCalls,
		}
		if info.Mintimes == nil {
			info.Mintimes = []string{}
		}
		if info.Maxtimes == nil {
			info.Maxtimes = []string{}
		}

		// TODO(bobg): this could just be info.Args = clause.params, if we
		// rejigger the types and exports.
		for _, p := range clause.params {
			info.Args = append(info.Args, ClauseArg{Name: p.name, Typ: string(p.bestType())})
		}
		for _, stmt := range clause.statements {
			switch s := stmt.(type) {
			case *lockStatement:
				valueInfo := ValueInfo{
					Name:    s.locked.String(),
					Program: s.program.String(),
				}
				if s.locked.String() != c.value {
					for _, r := range clause.reqs {
						if s.locked.String() == r.name {
							valueInfo.Asset = r.assetExpr.String()
							valueInfo.Amount = r.amountExpr.String()
							break
						}
					}
				}
				info.Values = append(info.Values, valueInfo)
			case *unlockStatement:
				valueInfo := ValueInfo{Name: c.value}
				info.Values = append(info.Values, valueInfo)
			}
		}
		result.Clauses = append(result.Clauses, info)
	}
	return result, nil
}

func compileContract(contract *contract, args []ContractArg) ([]byte, error) {
	if len(contract.clauses) == 0 {
		return nil, fmt.Errorf("empty contract")
	}

	env := newEnviron(nil)
	for _, k := range keywords {
		env.add(k, nilType, roleKeyword)
	}
	for _, b := range builtins {
		env.add(b.name, nilType, roleBuiltin)
	}
	err := env.add(contract.name, contractType, roleContract)
	if err != nil {
		return nil, err
	}
	for _, p := range contract.params {
		err = env.add(p.name, p.typ, roleContractParam)
		if err != nil {
			return nil, err
		}
	}
	err = env.add(contract.value, valueType, roleContractValue)
	if err != nil {
		return nil, err
	}
	for _, c := range contract.clauses {
		err = env.add(c.name, nilType, roleClause)
		if err != nil {
			return nil, err
		}
	}

	err = prohibitValueParams(contract)
	if err != nil {
		return nil, err
	}
	err = requireAllParamsUsedInClauses(contract.params, contract.clauses)
	if err != nil {
		return nil, err
	}

	stack := addParamsToStack(nil, contract.params)

	b := newBuilder()
	for _, a := range args {
		switch {
		case a.B != nil:
			var n int64
			if *a.B {
				n = 1
			}
			b.addInt64(n)
		case a.I != nil:
			b.addInt64(*a.I)
		case a.S != nil:
			b.addData(*a.S)
		}
	}

	if len(contract.clauses) == 1 {
		err = compileClause(b, stack, contract, env, contract.clauses[0])
		if err != nil {
			return nil, err
		}
		return b.build()
	}

	endTarget := b.newJumpTarget()
	clauseTargets := make([]int, len(contract.clauses))
	for i := range contract.clauses {
		clauseTargets[i] = b.newJumpTarget()
	}

	if len(stack) > 0 {
		// A clause selector is at the bottom of the stack. Roll it to the
		// top.
		b.addInt64(int64(len(stack)))
		b.addOp(vm.OP_ROLL) // stack: [<clause params> <contract params> <clause selector>]
	}

	// clauses 2..N-1
	for i := len(contract.clauses) - 1; i >= 2; i-- {
		b.addOp(vm.OP_DUP)            // stack: [... <clause selector> <clause selector>]
		b.addInt64(int64(i))          // stack: [... <clause selector> <clause selector> <i>]
		b.addOp(vm.OP_NUMEQUAL)       // stack: [... <clause selector> <i == clause selector>]
		b.addJumpIf(clauseTargets[i]) // stack: [... <clause selector>]
	}

	// clause 1
	b.addJumpIf(clauseTargets[1])

	// no jump needed for clause 0

	for i, clause := range contract.clauses {
		b.setJumpTarget(clauseTargets[i])

		// An inner builder is used for each clause body in order to get
		// any final VERIFY instruction left off, and the bytes of its
		// program appended to the outer builder.
		//
		// (Building the clause in the outer builder, then adding a JUMP
		// to endTarget, would cause the omitted VERIFY to be added.)
		//
		// This only works as long as the inner program contains no jumps,
		// whose absolute addresses would be invalidated by this
		// operation. Luckily we don't generate jumps in clause
		// bodies... yet.
		//
		// TODO(bobg): when we _do_ generate jumps in clause bodies, we'll
		// need a cleverer way to remove the trailing VERIFY.
		b2 := newBuilder()
		err = compileClause(b2, stack, contract, env, clause)
		if err != nil {
			return nil, errors.Wrapf(err, "compiling clause %d", i)
		}
		prog, err := b2.build()
		if err != nil {
			return nil, errors.Wrap(err, "assembling bytecode")
		}
		b.addRawBytes(prog)

		if i < len(contract.clauses)-1 {
			b.addJump(endTarget)
		}
	}
	b.setJumpTarget(endTarget)
	return b.build()
}

func compileClause(b *builder, contractStack []stackEntry, contract *contract, env *environ, clause *clause) error {
	// copy env to leave outerEnv unchanged
	env = newEnviron(env)
	for _, p := range clause.params {
		err := env.add(p.name, p.typ, roleClauseParam)
		if err != nil {
			return err
		}
	}
	for _, req := range clause.reqs {
		err := env.add(req.name, valueType, roleClauseValue)
		if err != nil {
			return err
		}
	}

	err := requireAllValuesDisposedOnce(contract, clause)
	if err != nil {
		return err
	}
	err = typeCheckClause(contract, clause, env)
	if err != nil {
		return err
	}
	err = requireAllParamsUsedInClause(clause.params, clause)
	if err != nil {
		return err
	}
	assignIndexes(clause)
	stack := addParamsToStack(nil, clause.params)
	stack = append(stack, contractStack...)
	for _, s := range clause.statements {
		switch stmt := s.(type) {
		case *verifyStatement:
			err = compileExpr(b, stack, contract, clause, env, stmt.expr)
			if err != nil {
				return errors.Wrapf(err, "in verify statement in clause \"%s\"", clause.name)
			}
			b.addOp(vm.OP_VERIFY)

			// special-case reporting of certain function calls
			if c, ok := stmt.expr.(*call); ok && len(c.args) == 1 {
				if b := referencedBuiltin(c.fn); b != nil {
					switch b.name {
					case "before":
						clause.maxtimes = append(clause.maxtimes, c.args[0].String())
					case "after":
						clause.mintimes = append(clause.mintimes, c.args[0].String())
					}
				}
			}

		case *lockStatement:
			// index
			b.addInt64(stmt.index)

			// copy of stack allows stack itself to remain unchanged in the
			// next iteration of the statements loop
			ostack := append(stack, stackEntry(fmt.Sprintf("%d", stmt.index)))

			// refdatahash
			b.addData(nil)
			ostack = append(ostack, stackEntry("''"))

			// TODO: permit more complex expressions for locked,
			// like "lock x+y with foo" (?)

			if stmt.locked.String() == contract.value {
				// amount
				b.addOp(vm.OP_AMOUNT)
				ostack = append(ostack, stackEntry("<amount>"))

				// asset
				b.addOp(vm.OP_ASSET)
				ostack = append(ostack, stackEntry("<asset>"))
			} else {
				var req *clauseRequirement
				for _, r := range clause.reqs {
					if stmt.locked.String() == r.name {
						req = r
						break
					}
				}
				if req == nil {
					return fmt.Errorf("unknown value \"%s\" in lock statement in clause \"%s\"", stmt.locked, clause.name)
				}

				// amount
				err = compileExpr(b, ostack, contract, clause, env, req.amountExpr)
				if err != nil {
					return errors.Wrapf(err, "in lock statement in clause \"%s\"", clause.name)
				}
				ostack = append(ostack, stackEntry(req.amountExpr.String()))

				// asset
				err = compileExpr(b, ostack, contract, clause, env, req.assetExpr)
				if err != nil {
					return errors.Wrapf(err, "in lock statement in clause \"%s\"", clause.name)
				}
				ostack = append(ostack, stackEntry(req.assetExpr.String()))
			}

			// version
			b.addInt64(1)
			ostack = append(ostack, stackEntry("1"))

			// prog
			err = compileExpr(b, ostack, contract, clause, env, stmt.program)
			if err != nil {
				return errors.Wrapf(err, "in lock statement in clause \"%s\"", clause.name)
			}

			b.addOp(vm.OP_CHECKOUTPUT)
			b.addOp(vm.OP_VERIFY)

		case *unlockStatement:
			if len(clause.statements) == 1 {
				// This is the only statement in the clause, make sure TRUE is
				// on the stack.
				b.addOp(vm.OP_TRUE)
			}
		}
	}
	return nil
}

func compileExpr(b *builder, stack []stackEntry, contract *contract, clause *clause, env *environ, expr expression) error {
	switch e := expr.(type) {
	case *binaryExpr:
		lType := e.left.typ(env)
		if e.op.left != "" && lType != e.op.left {
			return fmt.Errorf("in \"%s\", left operand has type \"%s\", must be \"%s\"", e, lType, e.op.left)
		}

		rType := e.right.typ(env)
		if e.op.right != "" && rType != e.op.right {
			return fmt.Errorf("in \"%s\", right operand has type \"%s\", must be \"%s\"", e, rType, e.op.right)
		}

		switch e.op.op {
		case "==", "!=":
			if lType != rType {
				// Maybe one is Hash and the other is (more-specific-Hash subtype).
				// TODO(bobg): generalize this mechanism
				if lType == hashType && isHashSubtype(rType) {
					propagateType(contract, clause, env, rType, e.left)
				} else if rType == hashType && isHashSubtype(lType) {
					propagateType(contract, clause, env, lType, e.right)
				} else {
					return fmt.Errorf("type mismatch in \"%s\": left operand has type \"%s\", right operand has type \"%s\"", e, lType, rType)
				}
			}
			if lType == "Boolean" {
				return fmt.Errorf("in \"%s\": using \"%s\" on Boolean values not allowed", e, e.op.op)
			}
		}

		err := compileExpr(b, stack, contract, clause, env, e.left)
		if err != nil {
			return errors.Wrapf(err, "in left operand of \"%s\" expression", e.op.op)
		}
		err = compileExpr(b, append(stack, stackEntry(e.left.String())), contract, clause, env, e.right)
		if err != nil {
			return errors.Wrapf(err, "in right operand of \"%s\" expression", e.op.op)
		}
		ops, err := vm.Assemble(e.op.opcodes)
		if err != nil {
			return errors.Wrapf(err, "assembling bytecode in \"%s\" expression", e.op.op)
		}
		b.addRawBytes(ops)

	case *unaryExpr:
		if e.op.operand != "" && e.expr.typ(env) != e.op.operand {
			return fmt.Errorf("in \"%s\", operand has type \"%s\", must be \"%s\"", e, e.expr.typ(env), e.op.operand)
		}
		err := compileExpr(b, stack, contract, clause, env, e.expr)
		if err != nil {
			return errors.Wrapf(err, "in \"%s\" expression", e.op.op)
		}
		ops, err := vm.Assemble(e.op.opcodes)
		if err != nil {
			return errors.Wrapf(err, "assembling bytecode in \"%s\" expression", e.op.op)
		}
		b.addRawBytes(ops)

	case *call:
		bi := referencedBuiltin(e.fn)
		if bi == nil {
			if e.fn.typ(env) == contractType {
				if e.fn.String() != contract.name {
					return fmt.Errorf("calling other contracts not yet supported")
				}
				// xxx typecheck args
				b.addInt64(int64(len(e.args)))
				stack = append(stack, stackEntry("<arg count>"))
				b.addData(nil)
				stack = append(stack, stackEntry("<program>"))
				for i := len(e.args) - 1; i >= 0; i-- {
					err := compileExpr(b, stack, contract, clause, env, e.args[i])
					if err != nil {
						return errors.Wrap(err, "compiling contract call")
					}
					b.addOp(vm.OP_CATPUSHDATA)
				}
				b.addInt64(0)
				b.addOp(vm.OP_CHECKPREDICATE)
				return nil
			}
			return fmt.Errorf("unknown function \"%s\"", e.fn)
		}

		// type-checking
		if len(e.args) != len(bi.args) {
			return fmt.Errorf("wrong number of args for \"%s\": have %d, want %d", bi.name, len(e.args), len(bi.args))
		}
		for i, actual := range e.args {
			if bi.args[i] != "" && actual.typ(env) != bi.args[i] {
				return fmt.Errorf("argument %d to \"%s\" has type \"%s\", must be \"%s\"", i, bi.name, actual.typ(env), bi.args[i])
			}
		}

		// WARNING WARNING WOOP WOOP
		// special-case hack
		// WARNING WARNING WOOP WOOP
		if bi.name == "checkTxMultiSig" {
			// type checking should have done this for us, but just in case:
			if len(e.args) != 2 {
				// xxx err
			}
			newEntries, err := compileArg(b, stack, contract, clause, env, e.args[1])
			if err != nil {
				return err
			}

			// stack: [... sigM ... sig1 M]

			b.addOp(vm.OP_TOALTSTACK) // stack: [... sigM ... sig1]
			newEntries = newEntries[:len(newEntries)-1]

			b.addOp(vm.OP_TXSIGHASH) // stack: [... sigM ... sig1 txsighash]
			newEntries = append(newEntries, stackEntry("<txsighash>"))

			_, err = compileArg(b, append(stack, newEntries...), contract, clause, env, e.args[0])
			if err != nil {
				return err
			}

			// stack: [... sigM ... sig1 txsighash pubkeyN ... pubkey1 N]

			b.addOp(vm.OP_FROMALTSTACK) // stack: [... sigM ... sig1 txsighash pubkeyN ... pubkey1 N M]
			b.addOp(vm.OP_SWAP)         // stack: [... sigM ... sig1 txsighash pubkeyN ... pubkey1 M N]
			b.addOp(vm.OP_CHECKMULTISIG)
			return nil
		}

		for i := len(e.args) - 1; i >= 0; i-- {
			a := e.args[i]
			newEntries, err := compileArg(b, stack, contract, clause, env, a)
			if err != nil {
				return errors.Wrapf(err, "compiling argument %d in call expression", i)
			}
			stack = append(stack, newEntries...)
		}
		ops, err := vm.Assemble(bi.opcodes)
		if err != nil {
			return errors.Wrap(err, "assembling bytecode in call expression")
		}
		b.addRawBytes(ops)

		// special-case reporting
		switch bi.name {
		case "sha3", "sha256":
			clause.hashCalls = append(clause.hashCalls, hashCall{bi.name, e.args[0].String(), string(e.args[0].typ(env))})
		}

	case varRef:
		return compileRef(b, stack, e)

	case integerLiteral:
		b.addInt64(int64(e))

	case bytesLiteral:
		b.addData([]byte(e))

	case booleanLiteral:
		if e {
			b.addOp(vm.OP_TRUE)
		} else {
			b.addOp(vm.OP_FALSE)
		}

	case listExpr:
		// Lists are excluded here because they disobey the invariant of
		// this function: namely, that it increases the stack size by
		// exactly one. (A list pushes its items and its length on the
		// stack.) But they're OK as function-call arguments because the
		// function (presumably) consumes all the stack items added.
		return fmt.Errorf("encountered list outside of function-call context")
	}
	return nil
}

func compileArg(b *builder, stack []stackEntry, contract *contract, clause *clause, env *environ, expr expression) ([]stackEntry, error) {
	var newEntries []stackEntry

	if list, ok := expr.(listExpr); ok {
		for i := 0; i < len(list); i++ {
			elt := list[len(list)-i-1]
			err := compileExpr(b, stack, contract, clause, env, elt)
			if err != nil {
				return nil, err
			}
			newEntry := stackEntry(elt.String())
			newEntries = append(newEntries, newEntry)
			stack = append(stack, newEntry)
		}
		b.addInt64(int64(len(list)))
		newEntries = append(newEntries, stackEntry(fmt.Sprintf("%d", len(list))))
		return newEntries, nil
	}

	err := compileExpr(b, stack, contract, clause, env, expr)
	if err != nil {
		return nil, err
	}
	return []stackEntry{stackEntry(expr.String())}, nil
}

func compileRef(b *builder, stack []stackEntry, ref expression) error {
	for depth := 0; depth < len(stack); depth++ {
		if stack[len(stack)-depth-1].matches(ref) {
			switch depth {
			case 0:
				b.addOp(vm.OP_DUP)
			case 1:
				b.addOp(vm.OP_OVER)
			default:
				b.addInt64(int64(depth))
				b.addOp(vm.OP_PICK)
			}
			return nil
		}
	}
	return fmt.Errorf("undefined reference \"%s\"", ref)
}

func (a *ContractArg) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	err := json.Unmarshal(b, &m)
	if err != nil {
		return err
	}
	if r, ok := m["boolean"]; ok {
		var bval bool
		err = json.Unmarshal(r, &bval)
		if err != nil {
			return err
		}
		a.B = &bval
		return nil
	}
	if r, ok := m["integer"]; ok {
		var ival int64
		err = json.Unmarshal(r, &ival)
		if err != nil {
			return err
		}
		a.I = &ival
		return nil
	}
	r, ok := m["string"]
	if !ok {
		return fmt.Errorf("contract arg must define one of boolean, integer, string")
	}
	var sval chainjson.HexBytes
	err = json.Unmarshal(r, &sval)
	if err != nil {
		return err
	}
	a.S = &sval
	return nil
}