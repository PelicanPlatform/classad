package main

import (
	"fmt"

	"github.com/bbockelm/golang-classads/classad"
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

	fmt.Println("=== Demo Complete ===")
}
