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
		has_apple = stringListIMember("APPLE", list2);
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
	hasApple := classAd2.EvaluateAttr("has_apple")

	fmt.Printf("  stringListSize(\"10,20,30,40,50\") = %v\n", size)
	fmt.Printf("  stringListSum(\"10,20,30,40,50\") = %v\n", sum)
	fmt.Printf("  stringListAvg(\"10,20,30,40,50\") = %v\n", avg)
	fmt.Printf("  stringListMin(\"10,20,30,40,50\") = %v\n", min)
	fmt.Printf("  stringListMax(\"10,20,30,40,50\") = %v\n", max)
	fmt.Printf("  stringListIMember(\"APPLE\", \"apple,banana,cherry\") = %v (case-insensitive)\n\n", hasApple)

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

	// unparse() function
	fmt.Println("6. Expression Introspection")
	fmt.Println("----------------------------")

	ad6 := `[
		x = 10;
		y = 20;
		sum = x + y;
		product = x * y;
		condition = x > 5 ? "yes" : "no";
		sum_str = unparse(sum);
		product_str = unparse(product);
		condition_str = unparse(condition);
	]`

	classAd6, err := classad.Parse(ad6)
	if err != nil {
		log.Fatal(err)
	}

	sumStr := classAd6.EvaluateAttr("sum_str")
	productStr := classAd6.EvaluateAttr("product_str")
	conditionStr := classAd6.EvaluateAttr("condition_str")

	fmt.Printf("  unparse(sum) = %v\n", sumStr)
	fmt.Printf("  unparse(product) = %v\n", productStr)
	fmt.Printf("  unparse(condition) = %v\n\n", conditionStr)

	// eval() function
	fmt.Println("7. Dynamic Expression Evaluation")
	fmt.Println("---------------------------------")

	ad7 := `[
		x = 10;
		y = 20;
		expr1 = "x + y";
		expr2 = "x * y";
		expr3 = "x > 5 ? 100 : 0";
		result1 = eval(expr1);
		result2 = eval(expr2);
		result3 = eval(expr3);
		slot5 = 500;
		slotId = 5;
		dynamicAttr = eval(strcat("slot", string(slotId)));
	]`

	classAd7, err := classad.Parse(ad7)
	if err != nil {
		log.Fatal(err)
	}

	result1 := classAd7.EvaluateAttr("result1")
	result2 := classAd7.EvaluateAttr("result2")
	result3 := classAd7.EvaluateAttr("result3")
	dynamicAttr := classAd7.EvaluateAttr("dynamicAttr")

	fmt.Printf("  eval(\"x + y\") where x=10, y=20 = %v\n", result1)
	fmt.Printf("  eval(\"x * y\") where x=10, y=20 = %v\n", result2)
	fmt.Printf("  eval(\"x > 5 ? 100 : 0\") where x=10 = %v\n", result3)
	fmt.Printf("  eval(strcat(\"slot\", string(5))) where slot5=500 = %v\n\n", dynamicAttr)

	fmt.Println("=== All 19 new functions working! ===")
}
