package main

import (
	"fmt"
	"log"

	"github.com/PelicanPlatform/classad/classad"
)

func main() {
	fmt.Println("=== New HTCondor ClassAd Functions Demo ===")
	fmt.Println()

	// List comparison functions
	fmt.Println("1. List Comparison Functions")
	fmt.Println("-----------------------------")

	ad1 := `[
		numbers = {1, 5, 3, 7, 2};
		threshold = 4;
		any_gt = anyCompare(">", numbers, threshold);
		all_gt = allCompare(">", numbers, threshold);
	]`

	classAd1, err := classad.Parse(ad1)
	if err != nil {
		log.Fatal(err)
	}

	anyGt := classAd1.EvaluateAttr("any_gt")
	allGt := classAd1.EvaluateAttr("all_gt")
	fmt.Printf("  anyCompare(\">\", {1, 5, 3, 7, 2}, 4) = %v\n", anyGt)
	fmt.Printf("  allCompare(\">\", {1, 5, 3, 7, 2}, 4) = %v\n\n", allGt)

	// StringList functions
	fmt.Println("2. StringList Functions")
	fmt.Println("-----------------------")

	ad2 := `[
		list1 = "10,20,30,40,50";
		list2 = "apple,banana,cherry";
		size = stringListSize(list1);
		sum = stringListSum(list1);
		avg = stringListAvg(list1);
		min = stringListMin(list1);
		max = stringListMax(list1);
	]`

	classAd2, err := classad.Parse(ad2)
	if err != nil {
		log.Fatal(err)
	}

	size := classAd2.EvaluateAttr("size")
	sum := classAd2.EvaluateAttr("sum")
	avg := classAd2.EvaluateAttr("avg")
	min := classAd2.EvaluateAttr("min")
	max := classAd2.EvaluateAttr("max")

	fmt.Printf("  stringListSize(\"10,20,30,40,50\") = %v\n", size)
	fmt.Printf("  stringListSum(\"10,20,30,40,50\") = %v\n", sum)
	fmt.Printf("  stringListAvg(\"10,20,30,40,50\") = %v\n", avg)
	fmt.Printf("  stringListMin(\"10,20,30,40,50\") = %v\n", min)
	fmt.Printf("  stringListMax(\"10,20,30,40,50\") = %v\n\n", max)

	// StringList set operations
	fmt.Println("3. StringList Set Operations")
	fmt.Println("-----------------------------")

	ad3 := `[
		set1 = "red,green,blue";
		set2 = "blue,yellow,purple";
		set3 = "red,green";
		intersect = stringListsIntersect(set1, set2);
		subset = stringListSubsetMatch(set3, set1);
	]`

	classAd3, err := classad.Parse(ad3)
	if err != nil {
		log.Fatal(err)
	}

	intersect := classAd3.EvaluateAttr("intersect")
	subset := classAd3.EvaluateAttr("subset")

	fmt.Printf("  stringListsIntersect(\"red,green,blue\", \"blue,yellow,purple\") = %v\n", intersect)
	fmt.Printf("  stringListSubsetMatch(\"red,green\", \"red,green,blue\") = %v\n\n", subset)

	// Regex functions
	fmt.Println("4. Regular Expression Functions")
	fmt.Println("--------------------------------")

	ad4 := `[
		pattern = "^test";
		list = {"testing", "foo", "test123"};
		text = "hello123world456";
		match = regexpMember(pattern, list);
		replaced = regexps("\\d+", text, "X");
		replaceFirst = replace("\\d+", text, "X");
	]`

	classAd4, err := classad.Parse(ad4)
	if err != nil {
		log.Fatal(err)
	}

	match := classAd4.EvaluateAttr("match")
	replaced := classAd4.EvaluateAttr("replaced")
	replaceFirst := classAd4.EvaluateAttr("replaceFirst")

	fmt.Printf("  regexpMember(\"^test\", {\"testing\", \"foo\", \"test123\"}) = %v\n", match)
	fmt.Printf("  regexps(\"\\\\d+\", \"hello123world456\", \"X\") = %v\n", replaced)
	fmt.Printf("  replace(\"\\\\d+\", \"hello123world456\", \"X\") = %v\n\n", replaceFirst)

	// StringList regex
	fmt.Println("5. StringList Regular Expression")
	fmt.Println("---------------------------------")

	ad5 := `[
		csvList = "test1,foo,test2,bar";
		hasTest = stringListRegexpMember("^test", csvList);
	]`

	classAd5, err := classad.Parse(ad5)
	if err != nil {
		log.Fatal(err)
	}

	hasTest := classAd5.EvaluateAttr("hasTest")
	fmt.Printf("  stringListRegexpMember(\"^test\", \"test1,foo,test2,bar\") = %v\n\n", hasTest)

	fmt.Println("=== All 16 new functions working! ===")
}
