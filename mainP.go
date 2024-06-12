package main

import (
	"fmt"
	"sort"
)

type Person struct {
	age  uint8
	name string
}

type perList []Person

func (p perList) Less(i, j int) bool {
	return p[i].age < p[j].age
}

func main() {
	list := perList{
		{3, "f"},
		{2, "z"},
	}

	sort.Slice(list, list.Less)

	for _, model := range list {
		fmt.Println(model)
	}
}
