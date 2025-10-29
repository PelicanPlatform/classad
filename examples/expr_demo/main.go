package main

import (
	"fmt"
	"log"

	"github.com/PelicanPlatform/classad/classad"
)

func main() {
	fmt.Println("=== ClassAd Expression API Demo ===")

	// Example 1: Parsing and working with expressions
	fmt.Println("Example 1: Parsing expressions")
	expr, err := classad.ParseExpr("Cpus * 2 + Memory / 1024")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Parsed expression: %s\n", expr)

	// Evaluate the expression in a ClassAd context
	ad := classad.New()
	ad.InsertAttr("Cpus", 8)
	ad.InsertAttr("Memory", 16384)

	result := expr.Eval(ad)
	if result.IsInteger() {
		value, _ := result.IntValue()
		fmt.Printf("Result: %d\n", value)
	}
	fmt.Println()

	// Example 2: Using Lookup to get unevaluated expressions
	fmt.Println("Example 2: Lookup unevaluated expressions")
	formulaAd, _ := classad.Parse("[Formula = Cpus * 2; Result = Formula + 10]")

	if formula, ok := formulaAd.Lookup("Formula"); ok {
		fmt.Printf("Formula expression: %s\n", formula)

		// Copy formula to a different ClassAd
		targetAd := classad.New()
		targetAd.InsertAttr("Cpus", 16)
		targetAd.InsertExpr("Computation", formula)

		if computed, ok := targetAd.EvaluateAttrInt("Computation"); ok {
			fmt.Printf("Computed value in target: %d\n", computed)
		}
	}
	fmt.Println()

	// Example 3: Scoped evaluation with MY and TARGET
	fmt.Println("Example 3: Scoped evaluation with MY and TARGET")

	// Create a job ClassAd
	job := classad.New()
	job.InsertAttr("RequestCpus", 4)
	job.InsertAttr("RequestMemory", 8192)
	job.InsertAttrString("Type", "Job")

	// Create a machine ClassAd
	machine := classad.New()
	machine.InsertAttr("Cpus", 8)
	machine.InsertAttr("Memory", 16384)
	machine.InsertAttrString("Type", "Machine")

	// Parse requirements expression with MY and TARGET references
	reqExpr, err := classad.ParseExpr("MY.RequestCpus <= TARGET.Cpus && MY.RequestMemory <= TARGET.Memory")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Requirements: %s\n", reqExpr)

	// Evaluate in job context (MY=job, TARGET=machine)
	result = reqExpr.EvalWithContext(job, machine)
	if result.IsBool() {
		matches, _ := result.BoolValue()
		fmt.Printf("Job requirements satisfied: %v\n", matches)
	}

	// Evaluate in machine context (MY=machine, TARGET=job)
	machineReqExpr, _ := classad.ParseExpr("TARGET.RequestCpus <= MY.Cpus")
	result = machineReqExpr.EvalWithContext(machine, job)
	if result.IsBool() {
		accepts, _ := result.BoolValue()
		fmt.Printf("Machine accepts job: %v\n", accepts)
	}
	fmt.Println()

	// Example 4: Using EvaluateExprWithTarget method
	fmt.Println("Example 4: EvaluateExprWithTarget method")

	// Add requirements to job
	job.InsertExpr("Requirements", reqExpr)

	// Evaluate requirements with machine as target
	reqResult := job.EvaluateExprWithTarget(reqExpr, machine)
	if reqResult.IsBool() {
		satisfied, _ := reqResult.BoolValue()
		fmt.Printf("Requirements check: %v\n", satisfied)
	}
	fmt.Println()

	// Example 5: Complex matching scenario
	fmt.Println("Example 5: Complex job-machine matching")

	// Create more detailed job and machine
	detailedJob, _ := classad.Parse(`[
		JobId = 12345;
		Owner = "alice";
		RequestCpus = 4;
		RequestMemory = 8192;
		RequestDisk = 100000;
		Type = "Job"
	]`)

	detailedMachine, _ := classad.Parse(`[
		Name = "slot1@worker-01";
		Cpus = 8;
		Memory = 16384;
		Disk = 500000;
		Arch = "X86_64";
		Type = "Machine"
	]`)

	// Job requirements: machine must have enough resources
	jobReqExpr, _ := classad.ParseExpr(`
		(TARGET.Cpus >= MY.RequestCpus) &&
		(TARGET.Memory >= MY.RequestMemory) &&
		(TARGET.Disk >= MY.RequestDisk)
	`)

	// Machine requirements: job must not exceed capacity
	machReqExpr, _ := classad.ParseExpr(`
		(TARGET.RequestCpus <= MY.Cpus) &&
		(TARGET.RequestMemory <= MY.Memory)
	`)

	jobMatches := jobReqExpr.EvalWithContext(detailedJob, detailedMachine)
	machineMatches := machReqExpr.EvalWithContext(detailedMachine, detailedJob)

	if jobMatches.IsBool() && machineMatches.IsBool() {
		jobOk, _ := jobMatches.BoolValue()
		machineOk, _ := machineMatches.BoolValue()

		fmt.Printf("Job requirements satisfied: %v\n", jobOk)
		fmt.Printf("Machine requirements satisfied: %v\n", machineOk)

		if jobOk && machineOk {
			fmt.Println("✓ Job and machine match!")
		}
	}
	fmt.Println()

	// Example 6: Ranking expressions
	fmt.Println("Example 6: Ranking expressions")

	// Job ranks machines by available memory
	rankExpr, _ := classad.ParseExpr("TARGET.Memory - MY.RequestMemory")
	rank := detailedJob.EvaluateExprWithTarget(rankExpr, detailedMachine)

	if rank.IsInteger() {
		rankValue, _ := rank.IntValue()
		fmt.Printf("Machine rank (spare memory): %d MB\n", rankValue)
	}
	fmt.Println()

	// Example 7: Copying expressions between ClassAds
	fmt.Println("Example 7: Copying expressions between ClassAds")

	// Create a template with common expressions
	template, _ := classad.Parse(`[
		StandardRequirements = (Cpus >= 2) && (Memory >= 4096);
		StandardRank = Memory;
		ResourceCalc = Cpus * 1000 + Memory / 1024
	]`)

	// Create a new job and copy expressions from template
	newJob := classad.New()
	newJob.InsertAttr("Cpus", 4)
	newJob.InsertAttr("Memory", 8192)

	if stdReq, ok := template.Lookup("StandardRequirements"); ok {
		newJob.InsertExpr("Requirements", stdReq)
		fmt.Println("Copied requirements from template")
	}

	if stdRank, ok := template.Lookup("StandardRank"); ok {
		newJob.InsertExpr("Rank", stdRank)
		fmt.Println("Copied rank from template")
	}

	if resCalc, ok := template.Lookup("ResourceCalc"); ok {
		newJob.InsertExpr("Score", resCalc)
		fmt.Println("Copied resource calculation from template")
	}

	// Evaluate in new job context
	if req, ok := newJob.EvaluateAttrBool("Requirements"); ok {
		fmt.Printf("Requirements satisfied: %v\n", req)
	}
	if score, ok := newJob.EvaluateAttrInt("Score"); ok {
		fmt.Printf("Resource score: %d\n", score)
	}
	fmt.Println()

	fmt.Println("=== Demo Complete ===")
}
