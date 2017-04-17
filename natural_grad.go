package anyrl

import (
	"github.com/unixpickle/anydiff"
	"github.com/unixpickle/anydiff/anyfwd"
	"github.com/unixpickle/anydiff/anyseq"
	"github.com/unixpickle/anynet"
	"github.com/unixpickle/anynet/anyrnn"
	"github.com/unixpickle/lazyrnn"
	"github.com/unixpickle/serializer"
)

// Default number of iterations for Conjugate Gradients.
const DefaultConjGradIters = 10

// NaturalPG implements natural policy gradients.
// Due to requirements involivng second derivatives,
// NaturalPG requires more detailed access to the policy
// than does PolicyGradient.
type NaturalPG struct {
	Policy      anyrnn.Block
	Params      []*anydiff.Var
	ActionSpace ActionSpace

	// Iters specifies the number of iterations for which
	// to run Conjugate Gradients.
	// If 0, DefaultConjGradIters is used.
	Iters int

	// FwdDiff copies the Policy and changes it to use an
	// anyfwd.Creator with the derivatives given in g.
	// Any gradients missing from g should be set to 0.
	//
	// Since the resulting block will have a different set
	// of parameter pointers, this returns a mapping from
	// the new parameters to the old parameters.
	//
	// If nil, the package-level MakeFwdDiff is used.
	FwdDiff func(c *anyfwd.Creator, p anyrnn.Block, g anydiff.Grad) (anyrnn.Block,
		map[*anydiff.Var]*anydiff.Var)

	// ApplyPolicy applies a policy to an input sequence.
	// If nil, back-propagation through time is used.
	ApplyPolicy func(s lazyrnn.Rereader, b anyrnn.Block) lazyrnn.Seq
}

// Run computes the natural gradient for the rollouts.
func (n *NaturalPG) Run(r *RolloutSet) anydiff.Grad {
	grad := anydiff.NewGrad(n.Params...)
	if len(grad) == 0 {
		return grad
	}

	PolicyGradient(n.ActionSpace, r, grad, func(in lazyrnn.Rereader) lazyrnn.Seq {
		return n.apply(in, n.Policy)
	})

	// TODO: perform conjugate gradients here using applyFisher().
	panic("not yet implemented")

	return grad
}

func (n *NaturalPG) applyFisher(r *RolloutSet, grad anydiff.Grad,
	oldOuts lazyrnn.Tape) anydiff.Grad {
	c := &anyfwd.Creator{
		ValueCreator: creatorFromGrad(grad),
		GradSize:     1,
	}
	fwdOldOuts := &makeFwdTape{Tape: oldOuts, Creator: c}
	fwdBlock, paramMap := n.makeFwd(c, grad)
	fwdIn := &makeFwdTape{Tape: r.Inputs, Creator: c}

	outSeq := n.apply(lazyrnn.TapeRereader(c, fwdIn), fwdBlock)
	outRereader := lazyrnn.Lazify(lazyrnn.Unlazify(outSeq))
	klDiv := lazyrnn.Mean(lazyrnn.MapN(func(num int, v ...anydiff.Res) anydiff.Res {
		return n.ActionSpace.KL(v[0], v[1], num)
	}, lazyrnn.TapeRereader(c, fwdOldOuts), outRereader))

	newGrad := anydiff.Grad{}
	for newParam, oldParam := range paramMap {
		if _, ok := grad[oldParam]; ok {
			newGrad[newParam] = c.MakeVector(newParam.Vector.Len())
		}
	}

	one := c.MakeVector(1)
	one.AddScalar(c.MakeNumeric(1))
	klDiv.Propagate(one, newGrad)

	out := anydiff.Grad{}
	for newParam, paramGrad := range newGrad {
		out[paramMap[newParam]] = paramGrad.(*anyfwd.Vector).Jacobian[0]
	}

	return out
}

func (n *NaturalPG) apply(in lazyrnn.Rereader, b anyrnn.Block) lazyrnn.Seq {
	if n.ApplyPolicy == nil {
		cachedIn := lazyrnn.Unlazify(in)
		return lazyrnn.Lazify(anyrnn.Map(cachedIn, b))
	} else {
		return n.ApplyPolicy(in, b)
	}
}

func (n *NaturalPG) makeFwd(c *anyfwd.Creator, g anydiff.Grad) (anyrnn.Block,
	map[*anydiff.Var]*anydiff.Var) {
	if n.FwdDiff == nil {
		return MakeFwdDiff(c, n.Policy, g)
	} else {
		return n.FwdDiff(c, n.Policy, g)
	}
}

// MakeFwdDiff copies the NaturalPolicy, updates it to use
// forward automatic differentiation, and sets the forward
// derivatives to the vectors in g.
// It returns the new block and a mapping from the new
// parameters to the old ones.
//
// Copying is done by serializing the policy and then
// deserializing it once more.
// If the policy does not implement serializer.Serializer,
// this will fail.
//
// Setting the creator is done by looping through the
// policy parameters and updating all of their vectors.
// If there are hidden parameter vectors, this will fail.
func MakeFwdDiff(c *anyfwd.Creator, p anyrnn.Block, g anydiff.Grad) (anyrnn.Block,
	map[*anydiff.Var]*anydiff.Var) {
	data, err := serializer.SerializeAny(p)
	if err != nil {
		panic(err)
	}
	var newPolicy anyrnn.Block
	if err := serializer.DeserializeAny(data, &newPolicy); err != nil {
		panic(err)
	}

	oldParams := anynet.AllParameters(p)
	newToOld := map[*anydiff.Var]*anydiff.Var{}
	for i, param := range anynet.AllParameters(newPolicy) {
		oldParam := oldParams[i]
		newToOld[param] = oldParam
		newVec := c.MakeVector(param.Vector.Len()).(*anyfwd.Vector)
		newVec.Values.Set(param.Vector)
		if gradVec, ok := g[oldParam]; ok {
			newVec.Jacobian[0].Set(gradVec)
		}
		param.Vector = newVec
	}

	return newPolicy, newToOld
}

// makeFwdTape wraps a Tape to translate it to a forward
// auto-diff creator.
type makeFwdTape struct {
	Tape    lazyrnn.Tape
	Creator *anyfwd.Creator
}

func (m *makeFwdTape) ReadTape(start, end int) <-chan *anyseq.Batch {
	res := make(chan *anyseq.Batch, 1)
	go func() {
		for in := range m.Tape.ReadTape(start, end) {
			newBatch := &anyseq.Batch{
				Present: in.Present,
				Packed:  m.Creator.MakeVector(in.Packed.Len()),
			}
			newBatch.Packed.(*anyfwd.Vector).Values.Set(in.Packed)
			res <- newBatch
		}
	}()
	return res
}