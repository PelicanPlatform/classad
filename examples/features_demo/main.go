package main

import (
	"fmt"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

func main() {
	fmt.Println("=== ClassAd Advanced Features Demo ===")
	fmt.Println()

	// Example 1: Nested ClassAds
	fmt.Println("Example 1: Nested ClassAds")
	serverAd, _ := classad.Parse(`[
		name = "web-cluster";
		servers = {
			[hostname = "web1.example.com"; cpus = 4; memory = 8192],
			[hostname = "web2.example.com"; cpus = 8; memory = 16384],
			[hostname = "web3.example.com"; cpus = 4; memory = 8192]
		};
		totalServers = size(servers)
	]`)

	name, _ := serverAd.EvaluateAttrString("name")
	fmt.Printf("Cluster: %s\n", name)

	totalServers, _ := serverAd.EvaluateAttrInt("totalServers")
	fmt.Printf("Total servers: %d\n", totalServers)

	serversVal := serverAd.EvaluateAttr("servers")
	if serversVal.IsList() {
		servers, _ := serversVal.ListValue()
		fmt.Printf("Server details:\n")
		for i, server := range servers {
			if server.IsClassAd() {
				ad, _ := server.ClassAdValue()
				host, _ := ad.EvaluateAttrString("hostname")
				cpus, _ := ad.EvaluateAttrInt("cpus")
				mem, _ := ad.EvaluateAttrInt("memory")
				fmt.Printf("  %d. %s: %d CPUs, %d MB RAM\n", i+1, host, cpus, mem)
			}
		}
	}
	fmt.Println()

	// Example 2: IS and ISNT operators
	fmt.Println("Example 2: IS and ISNT operators")
	compareAd, _ := classad.Parse(`[
		intValue = 5;
		realValue = 5.0;

		equalCompare = (intValue == realValue);
		isCompare = (intValue is realValue);

		undefCheck = (undefined is undefined);
		typeCheck = ("hello" isnt 42)
	]`)

	equalComp, _ := compareAd.EvaluateAttrBool("equalCompare")
	isComp, _ := compareAd.EvaluateAttrBool("isCompare")
	fmt.Printf("5 == 5.0: %v (== allows type coercion)\n", equalComp)
	fmt.Printf("5 is 5.0: %v ('is' requires exact type match)\n", isComp)

	undefCheck, _ := compareAd.EvaluateAttrBool("undefCheck")
	fmt.Printf("undefined is undefined: %v\n", undefCheck)

	typeCheck, _ := compareAd.EvaluateAttrBool("typeCheck")
	fmt.Printf(`"hello" isnt 42: %v\n`, typeCheck)
	fmt.Println()

	// Example 3: String functions
	fmt.Println("Example 3: String functions")
	stringAd, _ := classad.Parse(`[
		firstName = "John";
		lastName = "Doe";
		fullName = strcat(firstName, " ", lastName);
		initials = strcat(substr(firstName, 0, 1), ".", substr(lastName, 0, 1), ".");
		nameLength = size(fullName);
		upperName = toUpper(fullName);
		lowerName = toLower("HELLO WORLD")
	]`)

	fullName, _ := stringAd.EvaluateAttrString("fullName")
	initials, _ := stringAd.EvaluateAttrString("initials")
	nameLen, _ := stringAd.EvaluateAttrInt("nameLength")
	upperName, _ := stringAd.EvaluateAttrString("upperName")
	lowerName, _ := stringAd.EvaluateAttrString("lowerName")

	fmt.Printf("Full name: %s\n", fullName)
	fmt.Printf("Initials: %s\n", initials)
	fmt.Printf("Name length: %d\n", nameLen)
	fmt.Printf("Uppercase: %s\n", upperName)
	fmt.Printf("Lowercase: %s\n", lowerName)
	fmt.Println()

	// Example 4: Math functions
	fmt.Println("Example 4: Math functions")
	mathAd, _ := classad.Parse(`[
		pi = 3.14159;
		e = 2.71828;

		piFloor = floor(pi);
		eCeiling = ceiling(e);
		roundPi = round(pi);

		convertToInt = int(3.9);
		convertToReal = real(42);

		random100 = random(100)
	]`)

	piFloor, _ := mathAd.EvaluateAttrInt("piFloor")
	eCeiling, _ := mathAd.EvaluateAttrInt("eCeiling")
	roundPi, _ := mathAd.EvaluateAttrInt("roundPi")
	convInt, _ := mathAd.EvaluateAttrInt("convertToInt")
	convReal, _ := mathAd.EvaluateAttrReal("convertToReal")
	rand100, _ := mathAd.EvaluateAttrReal("random100")

	fmt.Printf("floor(3.14159) = %d\n", piFloor)
	fmt.Printf("ceiling(2.71828) = %d\n", eCeiling)
	fmt.Printf("round(3.14159) = %d\n", roundPi)
	fmt.Printf("int(3.9) = %d\n", convInt)
	fmt.Printf("real(42) = %g\n", convReal)
	fmt.Printf("random(100) = %g\n", rand100)
	fmt.Println()

	// Example 5: Type checking functions
	fmt.Println("Example 5: Type checking functions")
	typeAd, _ := classad.Parse(`[
		stringVal = "hello";
		intVal = 42;
		realVal = 3.14;
		boolVal = true;
		listVal = {1, 2, 3};
		nestedAd = [x = 1];

		checkString = isString(stringVal);
		checkInt = isInteger(intVal);
		checkReal = isReal(realVal);
		checkBool = isBoolean(boolVal);
		checkList = isList(listVal);
		checkClassAd = isClassAd(nestedAd);
		checkUndef = isUndefined(missingAttr)
	]`)

	checkStr, _ := typeAd.EvaluateAttrBool("checkString")
	checkInt, _ := typeAd.EvaluateAttrBool("checkInt")
	checkReal, _ := typeAd.EvaluateAttrBool("checkReal")
	checkBool, _ := typeAd.EvaluateAttrBool("checkBool")
	checkList, _ := typeAd.EvaluateAttrBool("checkList")
	checkClassAd, _ := typeAd.EvaluateAttrBool("checkClassAd")
	checkUndef, _ := typeAd.EvaluateAttrBool("checkUndef")

	fmt.Printf("isString(\"hello\"): %v\n", checkStr)
	fmt.Printf("isInteger(42): %v\n", checkInt)
	fmt.Printf("isReal(3.14): %v\n", checkReal)
	fmt.Printf("isBoolean(true): %v\n", checkBool)
	fmt.Printf("isList({1,2,3}): %v\n", checkList)
	fmt.Printf("isClassAd([x=1]): %v\n", checkClassAd)
	fmt.Printf("isUndefined(missingAttr): %v\n", checkUndef)
	fmt.Println()

	// Example 6: List operations with member function
	fmt.Println("Example 6: List operations")
	listAd, _ := classad.Parse(`[
		allowedUsers = {"alice", "bob", "charlie", "david"};
		numbers = {1, 2, 3, 5, 8, 13, 21};

		hasAlice = member("alice", allowedUsers);
		hasEve = member("eve", allowedUsers);
		hasThirteen = member(13, numbers);
		hasFourteen = member(14, numbers);

		userCount = size(allowedUsers);
		numberCount = size(numbers)
	]`)

	hasAlice, _ := listAd.EvaluateAttrBool("hasAlice")
	hasEve, _ := listAd.EvaluateAttrBool("hasEve")
	hasThirteen, _ := listAd.EvaluateAttrBool("hasThirteen")
	hasFourteen, _ := listAd.EvaluateAttrBool("hasFourteen")
	userCount, _ := listAd.EvaluateAttrInt("userCount")
	numberCount, _ := listAd.EvaluateAttrInt("numberCount")

	fmt.Printf("member(\"alice\", allowedUsers): %v\n", hasAlice)
	fmt.Printf("member(\"eve\", allowedUsers): %v\n", hasEve)
	fmt.Printf("member(13, numbers): %v\n", hasThirteen)
	fmt.Printf("member(14, numbers): %v\n", hasFourteen)
	fmt.Printf("User count: %d\n", userCount)
	fmt.Printf("Number count: %d\n", numberCount)
	fmt.Println()

	// Example 7: Complex real-world scenario
	fmt.Println("Example 7: HTCondor job matching simulation")
	jobReq, _ := classad.Parse(`[
		JobId = 12345;
		Owner = "alice";
		RequestCpus = 4;
		RequestMemory = 8192;
		RequestDisk = 100000;
		Requirements = (Cpus >= RequestCpus) &&
		              (Memory >= RequestMemory) &&
		              (Disk >= RequestDisk) &&
		              member(Arch, {"X86_64", "ARM64"})
	]`)

	machine, _ := classad.Parse(`[
		Name = "slot1@worker.example.com";
		Cpus = 8;
		Memory = 16384;
		Disk = 500000;
		Arch = "X86_64";
		State = "Unclaimed"
	]`)

	// Evaluate job requirements in context of machine ad
	// For a real matching system, you'd merge the ClassAds first
	matchAd, _ := classad.Parse(`[
		JobRequestCpus = 4;
		JobRequestMemory = 8192;
		JobRequestDisk = 100000;
		MachineCpus = 8;
		MachineMemory = 16384;
		MachineDisk = 500000;
		MachineArch = "X86_64";

		cpuMatch = (MachineCpus >= JobRequestCpus);
		memMatch = (MachineMemory >= JobRequestMemory);
		diskMatch = (MachineDisk >= JobRequestDisk);
		archMatch = member(MachineArch, {"X86_64", "ARM64"});

		overallMatch = cpuMatch && memMatch && diskMatch && archMatch;

		matchScore = MachineCpus * 100 + (MachineMemory / 1024)
	]`)

	jobId, _ := jobReq.EvaluateAttrInt("JobId")
	owner, _ := jobReq.EvaluateAttrString("Owner")
	machineName, _ := machine.EvaluateAttrString("Name")

	cpuMatch, _ := matchAd.EvaluateAttrBool("cpuMatch")
	memMatch, _ := matchAd.EvaluateAttrBool("memMatch")
	diskMatch, _ := matchAd.EvaluateAttrBool("diskMatch")
	archMatch, _ := matchAd.EvaluateAttrBool("archMatch")
	overallMatch, _ := matchAd.EvaluateAttrBool("overallMatch")
	matchScore, _ := matchAd.EvaluateAttrInt("matchScore")

	fmt.Printf("Job %d (Owner: %s)\n", jobId, owner)
	fmt.Printf("Machine: %s\n", machineName)
	fmt.Printf("  CPU match: %v\n", cpuMatch)
	fmt.Printf("  Memory match: %v\n", memMatch)
	fmt.Printf("  Disk match: %v\n", diskMatch)
	fmt.Printf("  Arch match: %v\n", archMatch)
	fmt.Printf("Overall match: %v\n", overallMatch)
	fmt.Printf("Match score: %d\n", matchScore)
	fmt.Println()

	// Example 8: Meta-equal operators (=?= and =!=)
	fmt.Println("Example 8: Meta-equal operators (=?= and =!=)")
	metaAd, _ := classad.Parse(`[
		intVal = 5;
		realVal = 5.0;

		regularEqual = (intVal == realVal);
		metaEqual = (intVal =?= realVal);
		metaNotEqual = (intVal =!= realVal);

		sameTypeEqual = (5 =?= 5);
		undefCheck = (undefined =?= undefined)
	]`)

	regularEq, _ := metaAd.EvaluateAttrBool("regularEqual")
	metaEq, _ := metaAd.EvaluateAttrBool("metaEqual")
	metaNotEq, _ := metaAd.EvaluateAttrBool("metaNotEqual")
	sameTypeEq, _ := metaAd.EvaluateAttrBool("sameTypeEqual")
	undefMetaCheck, _ := metaAd.EvaluateAttrBool("undefCheck")

	fmt.Printf("5 == 5.0: %v (regular equality with type coercion)\n", regularEq)
	fmt.Printf("5 =?= 5.0: %v (meta-equal, requires exact type match)\n", metaEq)
	fmt.Printf("5 =!= 5.0: %v (meta-not-equal)\n", metaNotEq)
	fmt.Printf("5 =?= 5: %v (same type and value)\n", sameTypeEq)
	fmt.Printf("undefined =?= undefined: %v\n", undefMetaCheck)
	fmt.Println()

	// Example 9: Attribute selection expressions
	fmt.Println("Example 9: Attribute selection (record.field)")
	selectAd, _ := classad.Parse(`[
		employee = [
			name = "Jane Smith";
			id = 1234;
			department = [
				name = "Engineering";
				location = "Building A"
			]
		];
		empName = employee.name;
		empId = employee.id;
		deptName = employee.department.name;
		deptLoc = employee.department.location
	]`)

	empName, _ := selectAd.EvaluateAttrString("empName")
	empId, _ := selectAd.EvaluateAttrInt("empId")
	deptName, _ := selectAd.EvaluateAttrString("deptName")
	deptLoc, _ := selectAd.EvaluateAttrString("deptLoc")

	fmt.Printf("Employee: %s (ID: %d)\n", empName, empId)
	fmt.Printf("Department: %s, %s\n", deptName, deptLoc)
	fmt.Println()

	// Example 10: Subscript expressions
	fmt.Println("Example 10: Subscript expressions (list[index] and record[key])")
	subscriptAd, _ := classad.Parse(`[
		fruits = {"apple", "banana", "cherry", "date"};
		numbers = {10, 20, 30, 40, 50};
		matrix = {{1, 2, 3}, {4, 5, 6}, {7, 8, 9}};

		person = [name = "John"; age = 35; city = "Boston"];

		firstFruit = fruits[0];
		thirdFruit = fruits[2];
		secondNumber = numbers[1];
		matrixElement = matrix[1][2];

		personName = person["name"];
		personAge = person["age"]
	]`)

	firstFruit, _ := subscriptAd.EvaluateAttrString("firstFruit")
	thirdFruit, _ := subscriptAd.EvaluateAttrString("thirdFruit")
	secondNumber, _ := subscriptAd.EvaluateAttrInt("secondNumber")
	matrixElement, _ := subscriptAd.EvaluateAttrInt("matrixElement")
	personName, _ := subscriptAd.EvaluateAttrString("personName")
	personAge, _ := subscriptAd.EvaluateAttrInt("personAge")

	fmt.Printf("List indexing:\n")
	fmt.Printf("  fruits[0] = %s\n", firstFruit)
	fmt.Printf("  fruits[2] = %s\n", thirdFruit)
	fmt.Printf("  numbers[1] = %d\n", secondNumber)
	fmt.Printf("Nested list indexing:\n")
	fmt.Printf("  matrix[1][2] = %d\n", matrixElement)
	fmt.Printf("ClassAd subscripting:\n")
	fmt.Printf("  person[\"name\"] = %s\n", personName)
	fmt.Printf("  person[\"age\"] = %d\n", personAge)
	fmt.Println()

	// Example 11: Combined selection and subscripting
	fmt.Println("Example 11: Combined selection and subscripting")
	combinedAd, _ := classad.Parse(`[
		company = [
			name = "DataCorp";
			employees = {
				[name = "Alice"; salary = 100000],
				[name = "Bob"; salary = 95000],
				[name = "Charlie"; salary = 105000]
			}
		];
		firstEmp = company.employees[0];
		firstEmpName = company.employees[0].name;
		secondEmpSalary = company.employees[1].salary;
		avgSalary = (company.employees[0].salary +
		            company.employees[1].salary +
		            company.employees[2].salary) / 3
	]`)

	firstEmpVal := combinedAd.EvaluateAttr("firstEmp")
	if firstEmpVal.IsClassAd() {
		empAd, _ := firstEmpVal.ClassAdValue()
		name, _ := empAd.EvaluateAttrString("name")
		fmt.Printf("First employee (via variable): %s\n", name)
	}

	firstEmpName, _ := combinedAd.EvaluateAttrString("firstEmpName")
	secondEmpSalary, _ := combinedAd.EvaluateAttrInt("secondEmpSalary")
	avgSalary, _ := combinedAd.EvaluateAttrInt("avgSalary")

	fmt.Printf("First employee (via direct access): %s\n", firstEmpName)
	fmt.Printf("Second employee salary: $%d\n", secondEmpSalary)
	fmt.Printf("Average salary: $%d\n", avgSalary)
	fmt.Println()

	// Example 12: Scoped attribute references (MY., TARGET., PARENT.)
	fmt.Println("Example 12: Scoped attribute references")

	// Helper function to parse an expression by wrapping it in a ClassAd
	parseExpr := func(exprStr string) ast.Expr {
		tempAd, err := parser.ParseClassAd("[__temp = " + exprStr + "]")
		if err != nil {
			return nil
		}
		for _, attr := range tempAd.Attributes {
			if attr.Name == "__temp" {
				return attr.Value
			}
		}
		return nil
	}

	// Create job and machine ClassAds
	job := classad.New()
	job.InsertAttr("Cpus", 2)
	job.InsertAttr("Memory", 2048)
	job.Insert("Requirements", parseExpr("TARGET.Cpus >= MY.Cpus && TARGET.Memory >= MY.Memory"))

	machine1 := classad.New()
	machine1.InsertAttr("Cpus", 4)
	machine1.InsertAttr("Memory", 8192)
	machine1.InsertAttrString("Name", "worker1")

	machine2 := classad.New()
	machine2.InsertAttr("Cpus", 1)
	machine2.InsertAttr("Memory", 1024)
	machine2.InsertAttrString("Name", "worker2")

	// Test TARGET references with machine1
	job.SetTarget(machine1)
	match1, _ := job.EvaluateAttrBool("Requirements")
	m1Name, _ := machine1.EvaluateAttrString("Name")
	fmt.Printf("Job requires 2 CPUs, 2048 MB\n")
	fmt.Printf("  Machine %s has 4 CPUs, 8192 MB: Match = %v\n", m1Name, match1)

	// Test TARGET references with machine2
	job.SetTarget(machine2)
	match2, _ := job.EvaluateAttrBool("Requirements")
	m2Name, _ := machine2.EvaluateAttrString("Name")
	fmt.Printf("  Machine %s has 1 CPU, 1024 MB: Match = %v\n", m2Name, match2)

	// Demonstrate PARENT references
	parent := classad.New()
	parent.InsertAttr("MaxCpus", 8)

	child := classad.New()
	child.InsertAttr("Cpus", 4)
	child.Insert("CpuCheck", parseExpr("MY.Cpus <= PARENT.MaxCpus"))
	child.SetParent(parent)

	cpuCheck, _ := child.EvaluateAttrBool("CpuCheck")
	fmt.Printf("\nParent allows max 8 CPUs, child has 4 CPUs: %v\n", cpuCheck)
	fmt.Println()

	// Example 13: ClassAd matching with MatchClassAd
	fmt.Println("Example 13: ClassAd matching with MatchClassAd")

	// Create job ClassAd
	jobAd := classad.New()
	jobAd.InsertAttr("Cpus", 2)
	jobAd.InsertAttr("Memory", 2048)
	jobAd.InsertAttrString("Owner", "alice")
	jobAd.Insert("Requirements", parseExpr("TARGET.Cpus >= MY.Cpus && TARGET.Memory >= MY.Memory"))
	jobAd.Insert("Rank", parseExpr("TARGET.Memory"))

	// Create machine ClassAd
	machineAd := classad.New()
	machineAd.InsertAttr("Cpus", 4)
	machineAd.InsertAttr("Memory", 8192)
	machineAd.InsertAttrString("Name", "slot1@worker1")
	machineAd.Insert("Requirements", parseExpr("TARGET.Cpus <= MY.Cpus"))
	machineAd.Insert("Rank", parseExpr("1000 - TARGET.Memory"))

	// Create MatchClassAd (automatically sets up bidirectional TARGET references)
	matchClassAd := classad.NewMatchClassAd(jobAd, machineAd)

	// Check if job and machine match
	matches := matchClassAd.Match()
	jobOwner, _ := jobAd.EvaluateAttrString("Owner")
	machName, _ := machineAd.EvaluateAttrString("Name")

	fmt.Printf("Job (Owner: %s, Cpus: 2, Memory: 2048)\n", jobOwner)
	fmt.Printf("Machine (%s, Cpus: 4, Memory: 8192)\n", machName)
	fmt.Printf("Symmetric match (both Requirements satisfied): %v\n", matches)

	// Evaluate ranks from both perspectives
	if matches {
		jobRank, jobRankOk := matchClassAd.EvaluateRankLeft()
		machineRank, machineRankOk := matchClassAd.EvaluateRankRight()

		if jobRankOk && machineRankOk {
			fmt.Printf("Job rank (prefers more memory): %.2f\n", jobRank)
			fmt.Printf("Machine rank (prefers lighter jobs): %.2f\n", machineRank)
		}
	}

	// Demonstrate match failure
	fmt.Println("\nTesting with incompatible machine:")
	smallMachine := classad.New()
	smallMachine.InsertAttr("Cpus", 1)
	smallMachine.InsertAttr("Memory", 1024)
	smallMachine.InsertAttrString("Name", "slot1@small-worker")
	smallMachine.Insert("Requirements", parseExpr("TARGET.Cpus <= MY.Cpus"))

	matchClassAd.ReplaceRightAd(smallMachine)
	matchesSmall := matchClassAd.Match()
	smallName, _ := smallMachine.EvaluateAttrString("Name")
	fmt.Printf("Machine (%s, Cpus: 1, Memory: 1024)\n", smallName)
	fmt.Printf("Symmetric match: %v (job requires 2 CPUs)\n", matchesSmall)
	fmt.Println()

	fmt.Println("=== Demo Complete ===")
}
